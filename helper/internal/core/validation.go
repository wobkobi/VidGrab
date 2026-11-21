// YouTube URL allowlist/classification and playlist item-range parsing.
package core

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// URL kinds returned by Classify.
const (
	KindVideo    = "video"
	KindPlaylist = "playlist"
	KindChannel  = "channel"
)

// Go's "$" matches only at the very end of the text, never before a trailing newline, so a
// percent-decoded "%0A" can never slip through these.
var (
	// VideoIDRe matches an 11-character YouTube video ID.
	VideoIDRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	playlistIDRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{2,64}$`)
	channelIDRe   = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
	itemPartRe    = regexp.MustCompile(`^(\d+)(?:\s*-\s*(\d+))?$`)
	ipvFutureRe   = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)
	shortHosts    = map[string]bool{"youtu.be": true, "www.youtu.be": true}
	youtubeHosts  = map[string]bool{"youtube.com": true, "www.youtube.com": true, "m.youtube.com": true}
	videoPrefixes = map[string]bool{"shorts": true, "embed": true, "v": true, "live": true}
	// Channel tabs kept as-is; the channel root and "featured" map to /videos so yt-dlp returns
	// one flat list instead of a list of tabs
	channelTabs         = map[string]bool{"videos": true, "shorts": true, "streams": true}
	channelTabsAsVideos = map[string]bool{"": true, "featured": true}
)

const (
	// MaxURLLength bounds the URL accepted by Classify, in characters.
	MaxURLLength = 2048
	// MaxItemsSpecLength bounds an item selection string, in characters.
	MaxItemsSpecLength = 200
	// MaxItemIndex is the highest 1-based item position a selection may name.
	MaxItemIndex = 100_000
	// Handles are 3-30 chars in practice; the wider bound tolerates legacy names under /c/ and /user/
	maxChannelName = 100
	// Besides Unicode letters, marks and digits, handles may contain these (U+00B7 is the middle dot)
	channelNamePunctuation = "._-·"
)

// InvalidUrl means a URL is not an accepted YouTube video, playlist or channel URL.
type InvalidUrl struct{ Message string }

// Error returns the message shown to the user.
func (e *InvalidUrl) Error() string { return e.Message }

// InvalidItems means an item range such as "1-10,15" cannot be parsed.
type InvalidItems struct{ Message string }

// Error returns the message shown to the user.
func (e *InvalidItems) Error() string { return e.Message }

// ParsedUrl is a validated YouTube URL rebuilt from its checked parts.
type ParsedUrl struct {
	Kind      string // KindVideo, KindPlaylist or KindChannel
	ID        string
	Canonical string
}

// ItemRange is an inclusive 1-based range of playlist positions.
type ItemRange struct{ Start, End int }

func invalidURL(msg string) error { return &InvalidUrl{Message: msg} }

// IsVideoID reports whether s is a well-formed 11-character video ID.
func IsVideoID(s string) bool { return VideoIDRe.MatchString(s) }

// WatchUrl builds the canonical https://www.youtube.com/watch?v= URL for a video ID, or returns
// an *InvalidUrl when the ID is malformed.
func WatchUrl(videoID string) (string, error) {
	if !IsVideoID(videoID) {
		return "", invalidURL("Invalid video ID")
	}
	return "https://www.youtube.com/watch?v=" + videoID, nil
}

func videoURL(id string) (ParsedUrl, error) {
	canonical, err := WatchUrl(id)
	if err != nil {
		return ParsedUrl{}, err
	}
	return ParsedUrl{Kind: KindVideo, ID: id, Canonical: canonical}, nil
}

// Classify validates a YouTube URL and classifies it as a video, playlist or channel.
//
// The returned canonical URL is rebuilt from regex-checked parts, so nothing else from the
// input ever reaches yt-dlp. URL splitting is hand-rolled rather than net/url, so odd input
// (a stray "%", non-ASCII, repeated query keys) is handled predictably and rejected with a
// clear message. Errors are always *InvalidUrl.
func Classify(rawURL string) (ParsedUrl, error) {
	// A Go string can hold invalid UTF-8; it becomes U+FFFD, which no ID pattern or
	// channel-name rule accepts
	url := strings.TrimSpace(strings.ToValidUTF8(rawURL, "�"))
	if url == "" {
		return ParsedUrl{}, invalidURL("URL is required")
	}
	if utf8.RuneCountInString(url) > MaxURLLength || strings.IndexFunc(url, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
		return ParsedUrl{}, invalidURL("Not a valid YouTube URL")
	}
	parts, ok := urlSplit(url)
	if !ok || (parts.scheme != "http" && parts.scheme != "https") {
		return ParsedUrl{}, invalidURL("Not a valid YouTube URL")
	}
	if !parts.authorityOK() {
		return ParsedUrl{}, invalidURL("Not a valid YouTube URL")
	}
	host := parts.hostname()
	path := parts.path
	if path == "" {
		path = "/"
	}
	segments := splitSegments(path)

	if shortHosts[host] {
		if len(segments) == 1 && IsVideoID(segments[0]) {
			return videoURL(segments[0])
		}
		return ParsedUrl{}, invalidURL("Not a valid YouTube URL")
	}
	if !youtubeHosts[host] {
		return ParsedUrl{}, invalidURL("Only youtube.com and youtu.be URLs are supported")
	}

	query := parseQS(parts.query)
	switch {
	case path == "/watch" || path == "/watch/":
		videoID, present, err := single(query, "v")
		if err != nil {
			return ParsedUrl{}, err
		}
		if present {
			if IsVideoID(videoID) {
				return videoURL(videoID)
			}
			return ParsedUrl{}, invalidURL("Invalid video ID")
		}
		return playlistURL(query)
	case path == "/playlist" || path == "/playlist/":
		return playlistURL(query)
	case len(segments) == 2 && videoPrefixes[segments[0]]:
		if IsVideoID(segments[1]) {
			return videoURL(segments[1])
		}
		return ParsedUrl{}, invalidURL("Invalid video ID")
	}
	return channelURL(segments)
}

// splitResult holds the URL pieces (scheme, host part, path, query) that Classify uses.
type splitResult struct {
	scheme, netloc, path, query string
}

// urlSplit ports urllib.parse.urlsplit: a scheme is taken only when everything before the
// first ":" is a letter followed by scheme characters, the netloc runs from "//" to the first
// "/", "?" or "#", then the fragment and query are cut off. Unbalanced or invalid bracketed
// (IPv6) hosts fail, as urlsplit raises ValueError for them.
func urlSplit(url string) (splitResult, bool) {
	var res splitResult
	if i := strings.IndexByte(url, ':'); i > 0 && isASCIILetter(url[0]) {
		valid := true
		for _, c := range []byte(url[:i]) {
			if !isASCIILetter(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
				valid = false
				break
			}
		}
		if valid {
			res.scheme, url = strings.ToLower(url[:i]), url[i+1:]
		}
	}
	if strings.HasPrefix(url, "//") {
		end := len(url)
		if i := strings.IndexAny(url[2:], "/?#"); i >= 0 {
			end = i + 2
		}
		res.netloc, url = url[2:end], url[end:]
		open, closed := strings.Contains(res.netloc, "["), strings.Contains(res.netloc, "]")
		if open != closed {
			return res, false
		}
		if open && !bracketedHostOK(res.netloc) {
			return res, false
		}
	}
	if i := strings.IndexByte(url, '#'); i >= 0 {
		url = url[:i]
	}
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url, res.query = url[:i], url[i+1:]
	}
	res.path = url
	return res, true
}

// bracketedHostOK ports urlsplit's check that a "[...]" host is an IPv6 address or an IPvFuture
// literal ("v1.x").
func bracketedHostOK(netloc string) bool {
	_, after, _ := strings.Cut(netloc, "[")
	host, _, _ := strings.Cut(after, "]")
	if strings.HasPrefix(host, "v") {
		return ipvFutureRe.MatchString(host)
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Is6()
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// hostInfo splits the netloc like urlsplit's hostname/port properties: userinfo ends at the
// last "@", and the port follows the first ":" (or the ":" after "]" for bracketed hosts).
func (s splitResult) hostInfo() (userinfo string, hasUser bool, host, port string) {
	hostinfo := s.netloc
	if i := strings.LastIndexByte(s.netloc, '@'); i >= 0 {
		userinfo, hasUser, hostinfo = s.netloc[:i], true, s.netloc[i+1:]
	}
	if _, bracketed, found := strings.Cut(hostinfo, "["); found {
		host, port, _ = strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(port, ":")
	} else {
		host, port, _ = strings.Cut(hostinfo, ":")
	}
	return userinfo, hasUser, host, port
}

func (s splitResult) hostname() string {
	_, _, host, _ := s.hostInfo()
	return strings.ToLower(host)
}

// authorityOK rejects a URL carrying a username, password or port. An empty username or
// password and port 0 count as absent, so "https://@host" and "https://host:0" pass, which is
// harmless since the canonical URL is rebuilt. A non-numeric or out-of-range port is rejected.
func (s splitResult) authorityOK() bool {
	userinfo, hasUser, _, port := s.hostInfo()
	if hasUser {
		user, password, _ := strings.Cut(userinfo, ":")
		if user != "" || password != "" {
			return false
		}
	}
	if port == "" {
		return true
	}
	if strings.Trim(port, "0123456789") != "" {
		return false
	}
	return strings.Trim(port, "0") == ""
}

func splitSegments(path string) []string {
	var segments []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	return segments
}

// parseQS ports urllib.parse.parse_qs with its defaults: only "&" separates pairs, pairs
// without "=" or with an empty value are dropped (keep_blank_values=False), and "+" plus
// percent escapes are decoded with invalid UTF-8 replaced.
func parseQS(query string) map[string][]string {
	result := map[string][]string{}
	if query == "" {
		return result
	}
	for _, pair := range strings.Split(query, "&") {
		name, value, found := strings.Cut(pair, "=")
		if !found || value == "" {
			continue
		}
		key, _ := percentUnquote(strings.ReplaceAll(name, "+", " "), false)
		val, _ := percentUnquote(strings.ReplaceAll(value, "+", " "), false)
		result[key] = append(result[key], val)
	}
	return result
}

// percentUnquote decodes %XX escapes. Each run of ASCII characters is decoded on its own and
// non-ASCII characters pass through, so a multi-byte escape split by a literal non-ASCII
// character is invalid. A "%" not followed by two hex digits stays literal. With strict set,
// invalid UTF-8 is an error; otherwise it becomes U+FFFD.
func percentUnquote(s string, strict bool) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var out strings.Builder
	start := 0
	flush := func(end int) error {
		if start >= end {
			return nil
		}
		decoded := unquoteToBytes(s[start:end])
		if !utf8.Valid(decoded) {
			if strict {
				return fmt.Errorf("invalid UTF-8 in %q", s[start:end])
			}
			decoded = []byte(strings.ToValidUTF8(string(decoded), "�"))
		}
		out.Write(decoded)
		return nil
	}
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		if err := flush(i); err != nil {
			return "", err
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		out.WriteString(s[i : i+size])
		i += size
		start = i
	}
	if err := flush(len(s)); err != nil {
		return "", err
	}
	return out.String(), nil
}

func unquoteToBytes(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			out = append(out, byte(b))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// percentQuote escapes a path segment: every UTF-8 byte except ASCII letters, digits
// and "_.-~" becomes an uppercase %XX escape.
func percentQuote(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isASCIILetter(c) || (c >= '0' && c <= '9') || strings.IndexByte("_.-~", c) >= 0 {
			out.WriteByte(c)
		} else {
			fmt.Fprintf(&out, "%%%02X", c)
		}
	}
	return out.String()
}

// single returns a query parameter that must appear at most once; present is false when absent.
func single(query map[string][]string, key string) (value string, present bool, err error) {
	values := query[key]
	if len(values) == 0 {
		return "", false, nil
	}
	if len(values) > 1 {
		return "", false, invalidURL(fmt.Sprintf("Duplicate '%s' parameter", key))
	}
	return values[0], true, nil
}

// playlistURL classifies a /playlist or /watch URL that carries only a list= parameter.
func playlistURL(query map[string][]string) (ParsedUrl, error) {
	listID, present, err := single(query, "list")
	if err != nil {
		return ParsedUrl{}, err
	}
	if !present || !playlistIDRe.MatchString(listID) {
		return ParsedUrl{}, invalidURL("Invalid playlist URL")
	}
	return ParsedUrl{Kind: KindPlaylist, ID: listID, Canonical: "https://www.youtube.com/playlist?list=" + listID}, nil
}

// channelURL classifies /@handle, /channel/UC..., /c/name and /user/name URLs with an optional tab.
func channelURL(segments []string) (ParsedUrl, error) {
	if len(segments) == 0 {
		return ParsedUrl{}, invalidURL("Not a video, playlist or channel URL")
	}
	var base, ident string
	var rest []string
	first := segments[0]
	switch {
	case strings.HasPrefix(first, "@"):
		handle, encoded, err := channelName(first[1:], "Invalid channel handle")
		if err != nil {
			return ParsedUrl{}, err
		}
		base, ident, rest = "@"+encoded, "@"+handle, segments[1:]
	case first == "channel" && len(segments) >= 2:
		if !channelIDRe.MatchString(segments[1]) {
			return ParsedUrl{}, invalidURL("Invalid channel ID")
		}
		base, ident, rest = "channel/"+segments[1], segments[1], segments[2:]
	case (first == "c" || first == "user") && len(segments) >= 2:
		name, encoded, err := channelName(segments[1], "Invalid channel name")
		if err != nil {
			return ParsedUrl{}, err
		}
		base, ident, rest = first+"/"+encoded, name, segments[2:]
	default:
		return ParsedUrl{}, invalidURL("Not a video, playlist or channel URL")
	}

	if len(rest) > 1 {
		return ParsedUrl{}, invalidURL("Unsupported channel page")
	}
	tab := ""
	if len(rest) == 1 {
		tab = strings.ToLower(rest[0])
	}
	if channelTabsAsVideos[tab] {
		tab = "videos"
	} else if !channelTabs[tab] {
		return ParsedUrl{}, invalidURL("Unsupported channel page")
	}
	return ParsedUrl{Kind: KindChannel, ID: ident, Canonical: "https://www.youtube.com/" + base + "/" + tab}, nil
}

// channelName validates a channel handle or legacy name, which may be percent-encoded Unicode.
//
// The segment is decoded once and must then start with a letter, digit or underscore and hold
// only letters, combining marks, digits and channelNamePunctuation. That rules out "/", "%",
// "?" and dot-only names such as "..", so the re-encoded name cannot add path segments or
// dot-segments to the canonical URL. Returns the decoded name and its percent-encoded form.
func channelName(segment, message string) (name, encoded string, err error) {
	name, err = percentUnquote(segment, true)
	if err != nil {
		return "", "", invalidURL(message)
	}
	runes := []rune(name)
	if len(runes) == 0 || len(runes) > maxChannelName {
		return "", "", invalidURL(message)
	}
	if runes[0] != '_' && !unicode.IsLetter(runes[0]) && !unicode.IsNumber(runes[0]) {
		return "", "", invalidURL(message)
	}
	for _, r := range runes {
		if !strings.ContainsRune(channelNamePunctuation, r) && !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.IsNumber(r) {
			return "", "", invalidURL(message)
		}
	}
	return name, percentQuote(name), nil
}

// ParseItems parses a 1-based item selection such as "1-10,15" into sorted, merged inclusive
// ranges. A blank spec means "all items" and returns nil. Errors are *InvalidItems for a
// malformed, reversed, zero-based or too large spec.
func ParseItems(spec string) ([]ItemRange, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(spec) > MaxItemsSpecLength {
		return nil, &InvalidItems{Message: "Item range is too long"}
	}
	var ranges []ItemRange
	for _, raw := range strings.Split(spec, ",") {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}
		bad := &InvalidItems{Message: fmt.Sprintf("Invalid item range '%s'", part)}
		match := itemPartRe.FindStringSubmatch(part)
		if match == nil {
			return nil, bad
		}
		// Atoi fails only on overflow here, which is past MaxItemIndex anyway
		start, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, bad
		}
		end := start
		if match[2] != "" {
			if end, err = strconv.Atoi(match[2]); err != nil {
				return nil, bad
			}
		}
		if start < 1 || end < start || end > MaxItemIndex {
			return nil, bad
		}
		ranges = append(ranges, ItemRange{start, end})
	}
	if len(ranges) == 0 {
		return nil, &InvalidItems{Message: "Item range is empty"}
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start != ranges[j].Start {
			return ranges[i].Start < ranges[j].Start
		}
		return ranges[i].End < ranges[j].End
	})
	merged := []ItemRange{ranges[0]}
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r.Start <= last.End+1 {
			last.End = max(last.End, r.End)
		} else {
			merged = append(merged, r)
		}
	}
	return merged, nil
}

// FormatItems renders parsed ranges back to the normalised "1-10,15" form.
func FormatItems(ranges []ItemRange) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		if r.Start == r.End {
			parts[i] = strconv.Itoa(r.Start)
		} else {
			parts[i] = fmt.Sprintf("%d-%d", r.Start, r.End)
		}
	}
	return strings.Join(parts, ",")
}

// SelectItems picks the entries whose 1-based position falls inside any range, in their
// original order. Nil ranges select every entry.
func SelectItems[T any](entries []T, ranges []ItemRange) []T {
	if ranges == nil {
		return append([]T(nil), entries...)
	}
	var selected []T
	for i, entry := range entries {
		pos := i + 1
		for _, r := range ranges {
			if r.Start <= pos && pos <= r.End {
				selected = append(selected, entry)
				break
			}
		}
	}
	return selected
}
