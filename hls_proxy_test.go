package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRewritePlaylistRegistersEveryResource(t *testing.T) {
	proxy := NewHLSProxy(time.Hour)
	playlist := `#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="keys/video.key"
#EXT-X-MAP:URI='/init.mp4'
#EXTINF:10,
segment-1.ts
#EXT-X-STREAM-INF:BANDWIDTH=800000
https://cdn.example/low/index.m3u8
`
	rewritten, err := proxy.rewritePlaylist([]byte(playlist), "https://video.example/path/index.m3u8", "https://player.example/watch")
	if err != nil {
		t.Fatal(err)
	}
	output := string(rewritten)
	if strings.Contains(output, "video.example") || strings.Contains(output, "cdn.example") {
		t.Fatalf("upstream URL leaked into playlist: %s", output)
	}
	if count := strings.Count(output, "/hls/"); count != 4 {
		t.Fatalf("expected 4 rewritten resources, got %d: %s", count, output)
	}
	if len(proxy.resources) != 4 {
		t.Fatalf("expected 4 registered resources, got %d", len(proxy.resources))
	}
}

func TestMasterPlaylistExposesQualityURLs(t *testing.T) {
	const referer = "https://player.example/watch"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720\n720/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\n1080/index.m3u8\n")
		case "/720/index.m3u8", "/1080/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:5,\nsegment.ts\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy := NewHLSProxy(time.Hour)
	id, err := proxy.Register(upstream.URL+"/master.m3u8", referer)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/hls/"+id+"/master.m3u8", nil)
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("master status %d: %s", response.Code, response.Body.String())
	}
	want720 := "/hls/" + id + "/720p/index.m3u8"
	want1080 := "/hls/" + id + "/1080p/index.m3u8"
	if !strings.Contains(response.Body.String(), want720) || !strings.Contains(response.Body.String(), want1080) {
		t.Fatalf("quality URLs missing from master playlist: %s", response.Body.String())
	}

	variantRequest := httptest.NewRequest(http.MethodGet, want720, nil)
	variantResponse := httptest.NewRecorder()
	proxy.ServeHTTP(variantResponse, variantRequest)
	if variantResponse.Code != http.StatusOK || !strings.Contains(variantResponse.Body.String(), "/hls/") {
		t.Fatalf("variant response %d: %s", variantResponse.Code, variantResponse.Body.String())
	}
}

func TestHLSProxyPlaylistAndSegment(t *testing.T) {
	const referer = "https://player.example/watch"
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	token, err := proxy.Register(upstream.URL+"/index.m3u8", referer)
	if err != nil {
		t.Fatal(err)
	}
	playlistRequest := httptest.NewRequest(http.MethodGet, "/hls/"+token, nil)
	playlistResponse := httptest.NewRecorder()
	proxy.ServeHTTP(playlistResponse, playlistRequest)
	if playlistResponse.Code != http.StatusOK {
		t.Fatalf("playlist status %d: %s", playlistResponse.Code, playlistResponse.Body.String())
	}
	line := ""
	for _, candidate := range strings.Split(playlistResponse.Body.String(), "\n") {
		if strings.HasPrefix(candidate, "/hls/") {
			line = candidate
		}
	}
	if line == "" {
		t.Fatalf("rewritten segment not found: %s", playlistResponse.Body.String())
	}
	segmentRequest := httptest.NewRequest(http.MethodGet, line, nil)
	segmentResponse := httptest.NewRecorder()
	proxy.ServeHTTP(segmentResponse, segmentRequest)
	if segmentResponse.Code != http.StatusOK || segmentResponse.Body.String() != "video-data" {
		t.Fatalf("segment response %d: %q", segmentResponse.Code, segmentResponse.Body.String())
	}
}

func TestExpiredHLSResourceReturnsGone(t *testing.T) {
	proxy := NewHLSProxy(-time.Second)
	token, err := proxy.Register("https://video.example/index.m3u8", "")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/hls/"+token, nil)
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d", response.Code)
	}
}
