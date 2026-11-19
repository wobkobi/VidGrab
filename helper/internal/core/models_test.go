package core

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// The expected details below were captured from the Python backend (FastAPI + Pydantic) for the
// same bodies, so the 400 "Invalid request" detail stays identical.
func TestDecodeJobRequestErrors(t *testing.T) {
	cases := []struct{ body, detail string }{
		{`{"options":null}`, "body.url: Field required; body.options: Input should be a valid dictionary or object to extract fields from"},
		{`[1]`, "body: Input should be a valid dictionary or object to extract fields from"},
		{`"str"`, "body: Input should be a valid dictionary or object to extract fields from"},
		{``, "body: Field required"},
		{`null`, "body: Field required"},
		{`{bad`, "body.1: JSON decode error"},
		{`{"url":"x"} x`, "body.12: JSON decode error"},
		{`{"url":"x","options":{"quality":1080.0,"kind":true}}`, "body.options.kind: Input should be 'video' or 'audio'; body.options.quality: Input should be 'best', '2160', '1440', '1080', '720', '480' or '360'"},
		{`{"url":"x","options":[]}`, "body.options: Input should be a valid dictionary or object to extract fields from"},
		{`{"url":5,"options":{"kind":"gif","quality":"999"},"items":3}`, "body.url: Input should be a valid string; body.options.kind: Input should be 'video' or 'audio'; body.options.quality: Input should be 'best', '2160', '1440', '1080', '720', '480' or '360'; body.items: Input should be a valid string"},
		{`{"url":"x","options":{"audio_bitrate":"64"}}`, "body.options.audio_bitrate: Input should be '320', '192' or '128'"},
		{`{"options":{}}`, "body.url: Field required"},
	}
	for _, c := range cases {
		_, err := DecodeJobRequest([]byte(c.body))
		var verr *ValidationError
		if !errors.As(err, &verr) || err.Error() != c.detail {
			t.Errorf("DecodeJobRequest(%s) error = %v\nwant %s", c.body, err, c.detail)
		}
	}
}

func TestDecodeJobRequestLengthLimits(t *testing.T) {
	long := `{"url":"` + strings.Repeat("a", MaxURLLength+1) + `"}`
	if _, err := DecodeJobRequest([]byte(long)); err == nil || err.Error() != "body.url: String should have at most 2048 characters" {
		t.Errorf("long url error = %v", err)
	}
	items := `{"url":"x","items":"` + strings.Repeat("1", MaxItemsLength+1) + `"}`
	if _, err := DecodeJobRequest([]byte(items)); err == nil || err.Error() != "body.items: String should have at most 200 characters" {
		t.Errorf("long items error = %v", err)
	}
}

func TestDecodeJobRequestDefaultsAndCoercion(t *testing.T) {
	req, err := DecodeJobRequest([]byte(`{"url":"x","options":{"quality":1080,"audio_bitrate":320,"zzz":1},"unknown":true}`))
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultJobOptions()
	want.Quality, want.AudioBitrate = "1080", "320"
	if req.URL != "x" || req.Options != want || req.Items != nil {
		t.Fatalf("got %+v", req)
	}

	req, err = DecodeJobRequest([]byte(`{"url":"x","items":null}`))
	if err != nil || req.Items != nil || req.Options != DefaultJobOptions() {
		t.Fatalf("got %+v, %v", req, err)
	}
	req, err = DecodeJobRequest([]byte(`{"url":"x","items":"1,3"}`))
	if err != nil || req.Items == nil || *req.Items != "1,3" {
		t.Fatalf("got %+v, %v", req, err)
	}
}

func TestDecodeSettingsUpdate(t *testing.T) {
	valid := []struct {
		body string
		want SettingsUpdate
	}{
		{`{"concurrency":true,"fragments":"3","cookies":"yes","auto_update":1}`, SettingsUpdate{ptr(1), ptr(3), ptr(true), ptr(true)}},
		{`{"concurrency":3.0,"fragments":" 4 ","cookies":0.0,"auto_update":"0"}`, SettingsUpdate{ptr(3), ptr(4), ptr(false), ptr(false)}},
		{`{"concurrency":"3.0","cookies":0.0,"auto_update":"YES"}`, SettingsUpdate{Concurrency: ptr(3), Cookies: ptr(false), AutoUpdate: ptr(true)}},
		{`{}`, SettingsUpdate{}},
		{`{"concurrency":null,"cookies":null}`, SettingsUpdate{}},
	}
	for _, c := range valid {
		got, err := DecodeSettingsUpdate([]byte(c.body))
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("DecodeSettingsUpdate(%s) = %+v, %v", c.body, got, err)
		}
	}

	invalid := []struct{ body, detail string }{
		{`{"concurrency":9,"fragments":0,"cookies":"maybe","auto_update":2}`, "body.concurrency: Input should be less than or equal to 5; body.fragments: Input should be greater than or equal to 1; body.cookies: Input should be a valid boolean, unable to interpret input; body.auto_update: Input should be a valid boolean, unable to interpret input"},
		{`{"concurrency":2.5,"fragments":"x","cookies":[]}`, "body.concurrency: Input should be a valid integer, got a number with a fractional part; body.fragments: Input should be a valid integer, unable to parse string as an integer; body.cookies: Input should be a valid boolean"},
		{`{"concurrency":1e30,"fragments":"99999999999999999999999"}`, "body.concurrency: Unable to parse input string as an integer, exceeded maximum size; body.fragments: Input should be less than or equal to 8"},
		{`{"cookies":2.5}`, "body.cookies: Input should be a valid boolean"},
		{``, "body: Field required"},
	}
	for _, c := range invalid {
		_, err := DecodeSettingsUpdate([]byte(c.body))
		if err == nil || err.Error() != c.detail {
			t.Errorf("DecodeSettingsUpdate(%s) error = %v\nwant %s", c.body, err, c.detail)
		}
	}
}

func TestDecodeSettings(t *testing.T) {
	got, err := DecodeSettings([]byte(`{"concurrency":3,"cookies":true}`))
	if err != nil || got != (Settings{Concurrency: 3, Fragments: 4, Cookies: true, AutoUpdate: true}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, body := range []string{`{"concurrency":null}`, `{"fragments":99}`, `[]`, `garbage`} {
		got, err := DecodeSettings([]byte(body))
		if err == nil || got != DefaultSettings() {
			t.Errorf("DecodeSettings(%s) = %+v, %v; want defaults and an error", body, got, err)
		}
	}
}

func TestSettingsApplyAndJSON(t *testing.T) {
	merged := DefaultSettings().Apply(SettingsUpdate{Fragments: ptr(8), AutoUpdate: ptr(false)})
	data, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"concurrency":2,"fragments":8,"cookies":false,"auto_update":false}` {
		t.Fatalf("json = %s", data)
	}
	if merged.Validate() != nil {
		t.Fatal("merged settings should be valid")
	}
	if err := (Settings{Concurrency: 0, Fragments: 9}).Validate(); err == nil || err.Error() != "concurrency: Input should be greater than or equal to 1; fragments: Input should be less than or equal to 8" {
		t.Fatalf("Validate = %v", err)
	}
}

func TestJobOptionsJSONAndValidate(t *testing.T) {
	data, _ := json.Marshal(DefaultJobOptions())
	if string(data) != `{"kind":"video","quality":"best","codec":"any","container":"mp4","audio_format":"mp3","audio_bitrate":"192","max_size_mb":0}` {
		t.Fatalf("json = %s", data)
	}
	if DefaultJobOptions().Validate() != nil {
		t.Fatal("defaults should be valid")
	}
	bad := DefaultJobOptions()
	bad.Codec = "mpeg"
	if err := bad.Validate(); err == nil || err.Error() != "codec: Input should be 'any', 'h264', 'vp9' or 'av1'" {
		t.Fatalf("Validate = %v", err)
	}
	opts, err := DecodeJobOptions([]byte(`{"kind":"audio"}`))
	if err != nil || opts.Kind != "audio" || opts.AudioFormat != "mp3" {
		t.Fatalf("DecodeJobOptions = %+v, %v", opts, err)
	}
	bad = DefaultJobOptions()
	bad.MaxSizeMB = MaxSizeLimitMB + 1
	if err := bad.Validate(); err == nil || err.Error() != "max_size_mb: Input should be less than or equal to 4000" {
		t.Fatalf("Validate = %v", err)
	}
}

func TestDecodeMaxSize(t *testing.T) {
	cases := []struct {
		body   string
		want   int
		detail string
	}{
		{`{"url":"x"}`, 0, ""},
		{`{"url":"x","options":{"max_size_mb":10}}`, 10, ""},
		{`{"url":"x","options":{"max_size_mb":"25"}}`, 25, ""},
		{`{"url":"x","options":{"max_size_mb":null}}`, 0, ""},
		{`{"url":"x","options":{"max_size_mb":0}}`, 0, ""},
		{`{"url":"x","options":{"max_size_mb":4000}}`, 4000, ""},
		{`{"url":"x","options":{"max_size_mb":4001}}`, 0, "body.options.max_size_mb: Input should be less than or equal to 4000"},
		{`{"url":"x","options":{"max_size_mb":-1}}`, 0, "body.options.max_size_mb: Input should be greater than or equal to 0"},
		{`{"url":"x","options":{"max_size_mb":9.5}}`, 0, "body.options.max_size_mb: Input should be a valid integer, got a number with a fractional part"},
		{`{"url":"x","options":{"max_size_mb":"ten"}}`, 0, "body.options.max_size_mb: Input should be a valid integer, unable to parse string as an integer"},
	}
	for _, c := range cases {
		req, err := DecodeJobRequest([]byte(c.body))
		if c.detail != "" {
			if err == nil || err.Error() != c.detail {
				t.Errorf("%s: error %v, want %s", c.body, err, c.detail)
			}
			continue
		}
		if err != nil || req.Options.MaxSizeMB != c.want {
			t.Errorf("%s: got %d, %v", c.body, req.Options.MaxSizeMB, err)
		}
	}
}

func TestNeedsFfmpeg(t *testing.T) {
	cases := []struct {
		opts JobOptions
		want bool
	}{
		{JobOptions{Kind: "audio", AudioFormat: "mp3"}, true},
		{JobOptions{Kind: "audio", AudioFormat: "m4a"}, false},
		{JobOptions{Kind: "video", Quality: "best"}, true},
		{JobOptions{Kind: "video", Quality: "1080"}, true},
		{JobOptions{Kind: "video", Quality: "720"}, false},
		{JobOptions{Kind: "video", Quality: "360"}, false},
		{JobOptions{Kind: "video", Quality: "360", MaxSizeMB: 10}, true},
		{JobOptions{Kind: "audio", AudioFormat: "m4a", MaxSizeMB: 10}, false},
	}
	for _, c := range cases {
		if got := NeedsFfmpeg(c.opts); got != c.want {
			t.Errorf("NeedsFfmpeg(%+v) = %v, want %v", c.opts, got, c.want)
		}
	}
}
