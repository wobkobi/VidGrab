// Size limits (e.g. Discord uploads): choosing streams that already fit, and the bitrate maths
// for a re-encode when none do. Everything here is pure so it can be table tested.

package core

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	// BytesPerMB is the megabyte MaxSizeMB counts in. Upload limits are quoted in decimal
	// megabytes, and a decimal MB is the smaller of the two readings, so it is the safe one.
	BytesPerMB = 1_000_000

	// fitHeadroom is the share of the limit streams that already fit may use. yt-dlp's sizes
	// are sometimes estimates (filesize_approx) and the MP4 merge adds a little overhead, so
	// 3% is kept spare.
	fitHeadroom = 0.97
	// encodeHeadroom is the share of the limit a re-encode aims at. Rate control overshoots a
	// little and MP4 adds per-frame overhead, so 5% is kept spare; the size check afterwards
	// catches the rest.
	encodeHeadroom = 0.95

	// AudioKbps is the AAC bitrate of a re-encode; lowAudioKbps replaces it when the whole
	// budget is under lowAudioBelowKbps, where every audio kbit is worth more as video.
	AudioKbps         = 96
	lowAudioKbps      = 64
	lowAudioBelowKbps = 400
	// MinVideoKbps is the floor below which H.264 turns to mush even at 240p; a video that
	// would get less is refused rather than encoded into something unwatchable.
	MinVideoKbps = 100
)

// encodeHeights maps a video bitrate (kbps) to the tallest output that still looks clean at it,
// highest first. The steps follow common streaming ladders for H.264.
var encodeHeights = []struct{ minKbps, height int }{
	{4500, 1080},
	{2500, 720},
	{1200, 480},
	{600, 360},
	{0, 240},
}

// LimitBytes returns the byte limit for a MaxSizeMB value.
func LimitBytes(mb int) int64 { return int64(mb) * BytesPerMB }

// FitFormat is one yt-dlp format reduced to what size fitting needs. VCodec and ACodec are
// normalised (h264, vp9, av1; aac, opus) and empty when the format lacks that stream.
type FitFormat struct {
	ID     string
	VCodec string
	ACodec string
	Ext    string
	// Height is the shorter side of the frame, the measure yt-dlp's -S res sorts by.
	Height int
	// Size is filesize, else filesize_approx, in bytes; 0 when yt-dlp knows neither.
	Size float64
	// Audio is yt-dlp's own preference for the audio stream; zero for video-only formats.
	Audio AudioRank
}

// AudioRank is how yt-dlp itself ranks an audio stream: the original language over dubs and
// descriptive audio, plain over DRC (YouTube's dynamic range compressed copy), then the
// extractor's quality score.
type AudioRank struct {
	// LangPref is language_preference. yt-dlp treats a missing value as -1; YouTube gives the
	// original track 10, the default track 5 and descriptive audio -10.
	LangPref float64
	DRC      bool
	// Quality is the extractor's quality score, 0 when missing.
	Quality float64
}

// compareAudio returns +1 when a ranks above b on DRC then quality, -1 when below, 0 when
// equal. Language is compared separately because it outranks the video height.
func compareAudio(a, b AudioRank) int {
	switch {
	case a.DRC != b.DRC:
		if b.DRC {
			return 1
		}
		return -1
	case a.Quality > b.Quality:
		return 1
	case a.Quality < b.Quality:
		return -1
	}
	return 0
}

// audioRankOf reads the AudioRank fields of one raw yt-dlp format. DRC tracks carry "drc" in
// their format_id ("251-drc") and "DRC" in their format_note.
func audioRankOf(f map[string]any) AudioRank {
	rank := AudioRank{LangPref: -1}
	if pref, ok := asNumber(f["language_preference"]); ok {
		rank.LangPref = pref
	}
	if quality, ok := asNumber(f["quality"]); ok {
		rank.Quality = quality
	}
	id, _ := f["format_id"].(string)
	note, _ := f["format_note"].(string)
	rank.DRC = strings.Contains(strings.ToLower(id), "drc") || strings.Contains(strings.ToLower(note), "drc")
	return rank
}

// FitChoice is the stream combination SelectFit picked. Audio is empty for a progressive
// format that carries both streams.
type FitChoice struct {
	Video  string
	Audio  string
	Height int
	VCodec string
	ACodec string
	Size   float64
	// AudioRank is the rank of the audio stream (of the progressive format when Audio is "").
	AudioRank AudioRank
}

// Selector returns the yt-dlp -f value for the choice: "137+140" or "18".
func (c FitChoice) Selector() string {
	if c.Audio == "" {
		return c.Video
	}
	return c.Video + "+" + c.Audio
}

// normaliseAudioCodec maps yt-dlp audio codec strings (mp4a.40.2, opus) to aac/opus.
func normaliseAudioCodec(acodec string) string {
	codec := strings.ToLower(acodec)
	if strings.HasPrefix(codec, "mp4a") || strings.HasPrefix(codec, "aac") {
		return "aac"
	}
	head, _, _ := strings.Cut(codec, ".")
	return head
}

// FitFormats reads the formats list of full (not flat) yt-dlp JSON. Formats with neither a
// video nor an audio stream (storyboards) are dropped. A format without an id is kept for
// SourceHeight but never selected.
func FitFormats(raw map[string]any) []FitFormat {
	list, _ := raw["formats"].([]any)
	var formats []FitFormat
	for _, item := range list {
		f, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := f["format_id"].(string)
		vcodec, _ := f["vcodec"].(string)
		acodec, _ := f["acodec"].(string)
		format := FitFormat{ID: id, Size: sizeOf(f)}
		format.Ext, _ = f["ext"].(string)
		if vcodec != "" && vcodec != "none" {
			format.VCodec = normaliseCodec(vcodec)
			if height, ok := asInt(f["height"]); ok && height > 0 {
				if width, ok := asInt(f["width"]); ok && width > 0 {
					height = min(width, height)
				}
				format.Height = height
			}
		}
		if acodec != "" && acodec != "none" {
			format.ACodec = normaliseAudioCodec(acodec)
			format.Audio = audioRankOf(f)
		}
		if format.VCodec == "" && format.ACodec == "" {
			continue
		}
		formats = append(formats, format)
	}
	return formats
}

// InfoDuration returns the length in seconds from full yt-dlp JSON, or 0 when it is missing.
func InfoDuration(raw map[string]any) float64 {
	duration, ok := asNumber(raw["duration"])
	if !ok || !(duration > 0) {
		return 0
	}
	return duration
}

// QualityCap returns the height a Quality option caps at, or 0 for "best".
func QualityCap(quality string) int {
	height, err := strconv.Atoi(quality)
	if err != nil {
		return 0
	}
	return height
}

// PlaysAsIs reports whether a downloaded file can be kept without re-encoding: an MP4 (by
// ext, with or without the dot) holding H.264 video and AAC audio (or no audio), the pair
// every upload target plays. vcodec and acodec are yt-dlp codec strings ("avc1.4d401f",
// "mp4a.40.2", "none").
func PlaysAsIs(ext, vcodec, acodec string) bool {
	if !strings.EqualFold(strings.TrimPrefix(ext, "."), "mp4") || normaliseCodec(vcodec) != "h264" {
		return false
	}
	return acodec == "" || acodec == "none" || normaliseAudioCodec(acodec) == "aac"
}

// SelectFit picks the best H.264 + AAC stream combination whose known size is at most 97% of
// limitBytes, or reports false when none fits.
//
// Only H.264 with AAC is accepted because it is the pair every upload target plays; anything
// else is converted instead. Candidates are every H.264 video-only format paired with every
// AAC audio-only format, plus progressive H.264 + AAC MP4 formats on their own, all at most
// maxHeight tall (0 means no cap) and with a known size. The ranking is: the audio language
// yt-dlp prefers (a dub at 1080p is worse than the original at 720p), then tallest, then
// non-DRC and higher quality audio, then the larger total, which is the higher bitrate and so
// the better picture. Audio preference comes before size so a DRC or dubbed track that is a
// few bytes larger never wins.
func SelectFit(formats []FitFormat, maxHeight int, limitBytes int64) (FitChoice, bool) {
	budget := float64(limitBytes) * fitHeadroom
	var videos, audios []FitFormat
	var best FitChoice
	found := false
	consider := func(c FitChoice) {
		if c.Size > budget {
			return
		}
		if !found || betterFit(c, best) {
			best, found = c, true
		}
	}
	for _, f := range formats {
		if f.Size <= 0 || f.ID == "" {
			continue
		}
		switch {
		case f.VCodec == "" && f.ACodec == "aac":
			audios = append(audios, f)
		case f.VCodec != "h264" || f.Height <= 0 || (maxHeight > 0 && f.Height > maxHeight):
			continue
		case f.ACodec == "":
			videos = append(videos, f)
		case f.Ext == "mp4" && f.ACodec == "aac":
			consider(FitChoice{Video: f.ID, Height: f.Height, VCodec: f.VCodec, ACodec: f.ACodec, Size: f.Size, AudioRank: f.Audio})
		}
	}
	for _, v := range videos {
		for _, a := range audios {
			consider(FitChoice{
				Video: v.ID, Audio: a.ID, Height: v.Height,
				VCodec: v.VCodec, ACodec: a.ACodec, Size: v.Size + a.Size, AudioRank: a.Audio,
			})
		}
	}
	return best, found
}

// betterFit reports whether a ranks above b (see SelectFit).
func betterFit(a, b FitChoice) bool {
	if a.AudioRank.LangPref != b.AudioRank.LangPref {
		return a.AudioRank.LangPref > b.AudioRank.LangPref
	}
	if a.Height != b.Height {
		return a.Height > b.Height
	}
	if c := compareAudio(a.AudioRank, b.AudioRank); c != 0 {
		return c > 0
	}
	return a.Size > b.Size
}

// SourceHeight returns the tallest video stream at most maxHeight (0 means no cap), or 0 when
// no format reports a height in range.
func SourceHeight(formats []FitFormat, maxHeight int) int {
	tallest := 0
	for _, f := range formats {
		if f.VCodec != "" && (maxHeight == 0 || f.Height <= maxHeight) {
			tallest = max(tallest, f.Height)
		}
	}
	return tallest
}

// EncodePlan is how a video that does not fit is re-encoded.
type EncodePlan struct {
	// VideoKbps and AudioKbps are the target bitrates in kilobits (1000 bits) per second.
	VideoKbps int
	AudioKbps int
	// Height is the output's shorter side.
	Height int
	// CapFPS limits the frame rate to 30, which below 720p buys more per-frame quality than
	// smoothness is worth.
	CapFPS bool
}

// Scaled returns the plan with the video bitrate multiplied by factor (for the retry after
// an encode came out too big).
func (p EncodePlan) Scaled(factor float64) EncodePlan {
	p.VideoKbps = int(float64(p.VideoKbps) * factor)
	return p
}

// CappedToSource lowers the video bitrate to what a source of sourceBytes over duration
// seconds carries in total, less the plan's audio, so an encode never pads a small source up
// to the limit (constant-bitrate hardware encoders fill whatever they are given). It never
// goes below MinVideoKbps and leaves the plan alone when either input is unknown.
func (p EncodePlan) CappedToSource(sourceBytes int64, duration float64) EncodePlan {
	if sourceBytes <= 0 || !(duration > 0) {
		return p
	}
	sourceKbps := int(float64(sourceBytes) * 8 / duration / 1000)
	p.VideoKbps = max(min(p.VideoKbps, sourceKbps-p.AudioKbps), min(p.VideoKbps, MinVideoKbps))
	return p
}

// ErrUnknownDuration means yt-dlp reported no length (a live stream, say), so no bitrate can
// be worked out.
var ErrUnknownDuration = errors.New("Could not read the video length")

// TooLongError means a video cannot fit the size limit at any watchable bitrate.
type TooLongError struct {
	LimitMB int
	// Duration is the video's length and MaxDuration the longest that would fit, in seconds.
	Duration    float64
	MaxDuration float64
}

// Error returns "Too long to fit N MB".
func (e *TooLongError) Error() string { return fmt.Sprintf("Too long to fit %d MB", e.LimitMB) }

// Detail explains the limit for the job's detail field.
func (e *TooLongError) Detail() string {
	return fmt.Sprintf("At %d MB the longest video that fits is about %s; this one is %s.",
		e.LimitMB, FormatEta(e.MaxDuration), FormatEta(e.Duration))
}

// PlanEncode works out the bitrates and output height for re-encoding a video of duration
// seconds into limitMB megabytes. maxHeight is the tallest source available within the job's
// quality cap (0 when unknown); the output never goes above it.
//
// The budget is total_kbps = limit bits * 0.95 / duration / 1000. Audio takes 96 kbps (64 when
// the total is under 400 kbps) and video gets the rest. When that leaves video under
// MinVideoKbps the error is a *TooLongError whose MaxDuration is the length that would just
// reach the floor: limit bits * 0.95 / ((MinVideoKbps + 64) * 1000).
func PlanEncode(limitMB int, duration float64, maxHeight int) (EncodePlan, error) {
	if !(duration > 0) || math.IsInf(duration, 0) {
		return EncodePlan{}, ErrUnknownDuration
	}
	bits := float64(LimitBytes(limitMB)) * 8 * encodeHeadroom
	totalKbps := bits / duration / 1000
	audio := AudioKbps
	if totalKbps < lowAudioBelowKbps {
		audio = lowAudioKbps
	}
	video := int(math.Floor(totalKbps)) - audio
	if video < MinVideoKbps {
		return EncodePlan{}, &TooLongError{
			LimitMB:     limitMB,
			Duration:    duration,
			MaxDuration: bits / float64((MinVideoKbps+lowAudioKbps)*1000),
		}
	}
	height := 240
	for _, step := range encodeHeights {
		if video >= step.minKbps {
			height = step.height
			break
		}
	}
	if maxHeight > 0 {
		height = min(height, maxHeight)
	}
	return EncodePlan{VideoKbps: video, AudioKbps: audio, Height: height, CapFPS: height < 720}, nil
}
