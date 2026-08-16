package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRewritePlaylistUsesSessionRelativeNames(t *testing.T) {
	proxy := NewHLSProxy(time.Hour)
	id, err := proxy.CreateSession("https://video.example/path/index.m3u8", "https://player.example/watch")
	if err != nil {
		t.Fatal(err)
	}
	session, found := proxy.session(id)
	if !found {
		t.Fatal("session not found")
	}
	playlist := `#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="keys/video.key"
#EXT-X-MAP:URI='/init.mp4'
#EXTINF:10,
segment-1.ts
#EXT-X-STREAM-INF:BANDWIDTH=800000
https://cdn.example/low/index.m3u8
`
	rewritten, err := proxy.rewritePlaylist(session, []byte(playlist), "https://video.example/path/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	output := string(rewritten)
	if strings.Contains(output, "video.example") || strings.Contains(output, "cdn.example") {
		t.Fatalf("upstream URL leaked into playlist: %s", output)
	}
	for _, want := range []string{`URI="key-1.key"`, `URI="init-2.mp4"`, "segment-3.ts", "800k/" + variantPlaylistName} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected %q in rewritten playlist: %s", want, output)
		}
	}
	// entry playlist + four discovered resources
	if got := session.size(); got != 5 {
		t.Fatalf("expected 5 registered resources, got %d", got)
	}
}

func TestRewritePlaylistIsStableAcrossReloads(t *testing.T) {
	proxy := NewHLSProxy(time.Hour)
	id, _ := proxy.CreateSession("https://video.example/index.m3u8", "")
	session, _ := proxy.session(id)
	playlist := "#EXTM3U\n#EXTINF:5,\nsegment.ts\n"
	first, err := proxy.rewritePlaylist(session, []byte(playlist), "https://video.example/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	second, err := proxy.rewritePlaylist(session, []byte(playlist), "https://video.example/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("live reload produced different names:\n%s\n%s", first, second)
	}
	if got := session.size(); got != 2 {
		t.Fatalf("reload must not allocate new names, got %d resources", got)
	}
}

func TestHLSProxyPlaylistAndSegment(t *testing.T) {
	const referer = "https://player.example/watch"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != referer {
			t.Errorf("unexpected referer %q", r.Header.Get("Referer"))
		}
		switch r.URL.Path {
		case "/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:5,\nsegment.ts\n")
		case "/segment.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = io.WriteString(w, "video-data")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy := NewHLSProxy(time.Hour)
	id, err := proxy.CreateSession(upstream.URL+"/index.m3u8", referer)
	if err != nil {
		t.Fatal(err)
	}
	playlistPath := proxy.PlaylistPath(id)
	if !strings.HasSuffix(playlistPath, "/playlist.m3u8") {
		t.Fatalf("entry playlist must end in playlist.m3u8: %s", playlistPath)
	}

	playlistResponse := httptest.NewRecorder()
	proxy.ServeHTTP(playlistResponse, httptest.NewRequest(http.MethodGet, playlistPath, nil))
	if playlistResponse.Code != http.StatusOK {
		t.Fatalf("playlist status %d: %s", playlistResponse.Code, playlistResponse.Body.String())
	}
	if got := playlistResponse.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Fatalf("unexpected playlist content type %q", got)
	}
	name := ""
	for _, line := range strings.Split(playlistResponse.Body.String(), "\n") {
		if strings.HasSuffix(line, ".ts") {
			name = line
		}
	}
	if name == "" {
		t.Fatalf("rewritten segment not found: %s", playlistResponse.Body.String())
	}

	segmentResponse := httptest.NewRecorder()
	proxy.ServeHTTP(segmentResponse, httptest.NewRequest(http.MethodGet, "/hls/"+id+"/"+name, nil))
	if segmentResponse.Code != http.StatusOK || segmentResponse.Body.String() != "video-data" {
		t.Fatalf("segment response %d: %q", segmentResponse.Code, segmentResponse.Body.String())
	}
	if got := segmentResponse.Header().Get("Content-Type"); got != "video/mp2t" {
		t.Fatalf("unexpected segment content type %q", got)
	}
}

func TestHLSProxyForwardsRangeRequests(t *testing.T) {
	const payload = "0123456789"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=2-5" {
			t.Errorf("range header not forwarded: %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, payload[2:6])
	}))
	defer upstream.Close()

	proxy := NewHLSProxy(time.Hour)
	id, _ := proxy.CreateSession(upstream.URL+"/index.m3u8", "")
	session, _ := proxy.session(id)
	name, err := session.nameFor(upstream.URL+"/segment.ts", kindSegment)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/hls/"+id+"/"+name, nil)
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", response.Code)
	}
	if response.Body.String() != "2345" {
		t.Fatalf("unexpected body %q", response.Body.String())
	}
	if got := response.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range not propagated: %q", got)
	}
}

func TestHLSProxyRejectsUnknownAndMalformedPaths(t *testing.T) {
	proxy := NewHLSProxy(time.Hour)
	id, _ := proxy.CreateSession("https://video.example/index.m3u8", "")

	cases := []struct {
		path string
		code int
	}{
		{"/hls/" + id + "/playlist.m3u8", 0}, // valid, skipped below
		{"/hls/" + id + "/nope.ts", http.StatusNotFound},
		{"/hls/" + id + "/../secret", http.StatusNotFound},
		{"/hls/" + id, http.StatusNotFound},
		{"/hls/not-a-session/playlist.m3u8", http.StatusNotFound},
		{"/hls/" + strings.Repeat("a", 8) + "/playlist.m3u8", http.StatusGone},
	}
	for _, testCase := range cases {
		if testCase.code == 0 {
			continue
		}
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, httptest.NewRequest(http.MethodGet, testCase.path, nil))
		if response.Code != testCase.code {
			t.Errorf("%s: expected %d, got %d", testCase.path, testCase.code, response.Code)
		}
	}
}

func TestHLSProxyOptionsAndMethodNotAllowed(t *testing.T) {
	proxy := NewHLSProxy(time.Hour)
	id, _ := proxy.CreateSession("https://video.example/index.m3u8", "")

	options := httptest.NewRecorder()
	proxy.ServeHTTP(options, httptest.NewRequest(http.MethodOptions, proxy.PlaylistPath(id), nil))
	if options.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for preflight, got %d", options.Code)
	}
	if options.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Fatal("preflight must allow the Range header")
	}

	post := httptest.NewRecorder()
	proxy.ServeHTTP(post, httptest.NewRequest(http.MethodPost, proxy.PlaylistPath(id), nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") == "" {
		t.Fatalf("expected 405 with Allow header, got %d", post.Code)
	}
}

func TestExpiredSessionReturnsGone(t *testing.T) {
	proxy := NewHLSProxy(-time.Second)
	id, err := proxy.CreateSession("https://video.example/index.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodGet, proxy.PlaylistPath(id), nil))
	if response.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d", response.Code)
	}
}

func TestCreateSessionReusesExistingSession(t *testing.T) {
	proxy := NewHLSProxy(time.Hour)
	first, err := proxy.CreateSession("https://video.example/index.m3u8", "https://player.example/")
	if err != nil {
		t.Fatal(err)
	}
	second, err := proxy.CreateSession("https://video.example/index.m3u8", "https://player.example/")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("expected the same session to be reused, got %s and %s", first, second)
	}
	if len(proxy.sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(proxy.sessions))
	}
}

func TestQualityPathsServeEachVariant(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		switch r.URL.Path {
		case "/1080.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:5,\nhd.ts\n")
		case "/480.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:5,\nsd.ts\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy := NewHLSProxy(time.Hour)
	id, err := proxy.CreateSessionWithVariants(upstream.URL+"/master.m3u8", "", []VariantSource{
		{Label: "1080p", Resolution: "1920x1080", Height: 1080, Bandwidth: 5000000, URL: upstream.URL + "/1080.m3u8"},
		{Label: "480p", Resolution: "854x480", Height: 480, Bandwidth: 1200000, URL: upstream.URL + "/480.m3u8"},
	})
	if err != nil {
		t.Fatal(err)
	}

	master := httptest.NewRecorder()
	proxy.ServeHTTP(master, httptest.NewRequest(http.MethodGet, proxy.PlaylistPath(id), nil))
	if master.Code != http.StatusOK {
		t.Fatalf("master status %d: %s", master.Code, master.Body.String())
	}
	body := master.Body.String()
	for _, want := range []string{"1080p/index.m3u8", "480p/index.m3u8", "RESOLUTION=1920x1080", "BANDWIDTH=5000000"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in master playlist:\n%s", want, body)
		}
	}
	if strings.Index(body, "1080p/") > strings.Index(body, "480p/") {
		t.Fatalf("variants must be ordered best first:\n%s", body)
	}

	for label, segment := range map[string]string{"1080p": "hd.ts", "480p": "sd.ts"} {
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, httptest.NewRequest(http.MethodGet, proxy.VariantPath(id, label), nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status %d: %s", label, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), segment) {
			t.Fatalf("%s: upstream segment name must be rewritten:\n%s", label, response.Body.String())
		}
		// nested playlists must escape the quality directory
		if !strings.Contains(response.Body.String(), "../segment-") {
			t.Fatalf("%s: nested playlist must use ../ prefixed names:\n%s", label, response.Body.String())
		}
	}

	missing := httptest.NewRecorder()
	proxy.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, proxy.VariantPath(id, "2160p"), nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown quality must be 404, got %d", missing.Code)
	}
}

func TestQualityListEndpoint(t *testing.T) {
	proxy := NewHLSProxy(time.Hour)
	id, err := proxy.CreateSessionWithVariants("https://video.example/master.m3u8", "", []VariantSource{
		{Label: "720p", Height: 720, URL: "https://video.example/720.m3u8"},
		{Label: "1080p", Height: 1080, URL: "https://video.example/1080.m3u8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/hls/"+id+"/"+qualityListName, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("quality list status %d", response.Code)
	}
	var payload struct {
		Qualities []Variant `json:"qualities"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Qualities) != 2 || payload.Qualities[0].Label != "1080p" {
		t.Fatalf("unexpected quality list: %+v", payload.Qualities)
	}
	if payload.Qualities[0].Path != "1080p/"+variantPlaylistName {
		t.Fatalf("unexpected variant path %q", payload.Qualities[0].Path)
	}
}

func TestAutoQualityServesMasterPlaylist(t *testing.T) {
	proxy := NewHLSProxy(time.Hour)
	id, _ := proxy.CreateSessionWithVariants("https://video.example/master.m3u8", "", []VariantSource{
		{Label: "720p", Height: 720, URL: "https://video.example/720.m3u8"},
	})
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodGet, proxy.VariantPath(id, autoQuality), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("auto quality status %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "../720p/"+variantPlaylistName) {
		t.Fatalf("auto master must point back at the session root:\n%s", response.Body.String())
	}
}

func TestDuplicateQualityLabelsStayUnique(t *testing.T) {
	proxy := NewHLSProxy(time.Hour)
	id, err := proxy.CreateSessionWithVariants("https://video.example/master.m3u8", "", []VariantSource{
		{Label: "720p", Height: 720, Bandwidth: 2000000, URL: "https://video.example/a.m3u8"},
		{Label: "720p", Height: 720, Bandwidth: 1000000, URL: "https://video.example/b.m3u8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]bool{}
	for _, variant := range proxy.Qualities(id) {
		if labels[variant.Label] {
			t.Fatalf("duplicate label %q", variant.Label)
		}
		labels[variant.Label] = true
	}
	if len(labels) != 2 {
		t.Fatalf("expected 2 distinct labels, got %v", labels)
	}
}

func TestParseMasterPlaylistVariants(t *testing.T) {
	playlist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1200000,RESOLUTION=854x480
480/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
https://cdn.example/1080/index.m3u8
`
	variants, err := parseMasterPlaylist(playlist, "https://video.example/path/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 2 {
		t.Fatalf("expected 2 variants, got %+v", variants)
	}
	if variants[0].Label != "1080p" || variants[1].Label != "480p" {
		t.Fatalf("variants must be sorted best first: %+v", variants)
	}
	if variants[1].URL != "https://video.example/path/480/index.m3u8" {
		t.Fatalf("relative variant not resolved: %q", variants[1].URL)
	}
}

func TestParseMasterPlaylistMediaOnly(t *testing.T) {
	variants, err := parseMasterPlaylist("#EXTM3U\n#EXTINF:5,\nsegment.ts\n", "https://video.example/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 || variants[0].Label != autoQuality {
		t.Fatalf("media playlist must yield a single auto variant: %+v", variants)
	}
}
