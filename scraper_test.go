package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseShowPage(t *testing.T) {
	document := `<!doctype html><html><head>
		<title>Fallback title</title>
		<script>var halim_cfg = {"post_id":19430,"episode":2,"server":3};</script>
	</head><body>
		<h1>Mushen Ji: Test / Anime</h1>
		<select id="sequel_select_th">
			<option value="/mushen-ji-ep-1/">ตอนที่ 1</option>
		</select>
		<select class="episodes" id="sequel_select_en">
			<option value="https://www.anime108.com/mushen-ji-ep-2/">ตอนที่ <b>2</b></option>
		</select>
	</body></html>`

	metadata, err := NewScraper().ParseShowPage(document, "https://www.anime108.com/mushen-ji/")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.PostID != 19430 || metadata.Episode != 2 || metadata.Server != 3 {
		t.Fatalf("unexpected player metadata: %+v", metadata)
	}
	if metadata.Title != "Mushen Ji: Test / Anime" {
		t.Fatalf("unexpected title %q", metadata.Title)
	}
	if strings.ContainsAny(metadata.CleanTitle, `:/`) {
		t.Fatalf("unsafe clean title %q", metadata.CleanTitle)
	}
	if got := metadata.Episodes["Thai"]; len(got) != 1 || got[0].URL != "https://www.anime108.com/mushen-ji-ep-1/" {
		t.Fatalf("unexpected Thai episodes: %+v", got)
	}
	if got := metadata.Episodes["Sound Track"]; len(got) != 1 || got[0].Title != "ตอนที่ 2" {
		t.Fatalf("unexpected soundtrack episodes: %+v", got)
	}
}

func TestParseShowPageFallbacks(t *testing.T) {
	document := `<html><head><title>Anime &amp; More</title><link href="https://www.anime108.com/?p=42" rel="shortlink"></head></html>`
	metadata, err := NewScraper().ParseShowPage(document, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.PostID != 42 || metadata.Episode != 1 || metadata.Server != 1 {
		t.Fatalf("unexpected defaults: %+v", metadata)
	}
	if metadata.Title != "Anime & More" {
		t.Fatalf("unexpected decoded title %q", metadata.Title)
	}
	if metadata.Episodes["Thai"] == nil || metadata.Episodes["Sound Track"] == nil {
		t.Fatal("episode arrays must not be nil")
	}
}

func TestResolveStreamURLKeepsMasterPlaylist(t *testing.T) {
	const videoID = "video-123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/newplaylist/"+videoID+"/"+videoID+".m3u8" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\n360/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720\n720/index.m3u8\n")
	}))
	defer server.Close()

	got, err := NewScraper().ResolveStreamURL(context.Background(), server.URL+"/player?id="+videoID)
	if err != nil {
		t.Fatal(err)
	}
	want := server.URL + "/newplaylist/" + videoID + "/" + videoID + ".m3u8"
	if got != want {
		t.Fatalf("expected master playlist %q, got %q", want, got)
	}
}

func TestBalancedDivBlocks(t *testing.T) {
	document := `<div class="box featured"><div><span>first</span></div></div><div class="other"></div><div class='box'>second</div>`
	blocks := balancedDivBlocks(document, "box")
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks: %#v", len(blocks), blocks)
	}
	if !strings.Contains(blocks[0], "first") || !strings.Contains(blocks[1], "second") {
		t.Fatalf("wrong blocks: %#v", blocks)
	}
}
