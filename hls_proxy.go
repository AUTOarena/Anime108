package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	entryPlaylistName      = "playlist.m3u8"
	variantPlaylistName    = "index.m3u8"
	qualityListName        = "qualities.json"
	autoQuality            = "auto"
	maxResourcesPerSession = 20000
	maxVariantsPerSession  = 64
	maxSessions            = 512

	kindIndex   = "index"
	kindSegment = "segment"
	kindKey     = "key"
	kindInit    = "init"
)

// Variant is one selectable quality level of a session, addressable through
// /hls/{id}/{label}/index.m3u8.
type Variant struct {
	Label      string `json:"label"`
	Path       string `json:"path"`
	Resolution string `json:"resolution,omitempty"`
	Bandwidth  int    `json:"bandwidth,omitempty"`
	Height     int    `json:"height,omitempty"`
}

// VariantSource is a quality level discovered upstream before the session
// exists, used to seed a session with a synthesized master playlist.
type VariantSource struct {
	Label      string
	Resolution string
	Bandwidth  int
	Height     int
	URL        string
}

// hlsSession groups every upstream resource that belongs to one playback
// session. Resources are addressed by a generated file name so that the proxy
// exposes player friendly paths such as /hls/{id}/segment-12.ts while the
// upstream URL itself is never revealed to the client.
type hlsSession struct {
	id       string
	entryURL string
	referer  string

	mu          sync.Mutex
	expiresAt   time.Time
	byName      map[string]string
	byURL       map[string]string
	counter     int
	variants    []Variant
	variantName map[string]string
	labelByURL  map[string]string
	synthetic   bool
}

func newHLSSession(id, entryURL, referer string, expiresAt time.Time) *hlsSession {
	session := &hlsSession{
		id:          id,
		entryURL:    entryURL,
		referer:     referer,
		expiresAt:   expiresAt,
		byName:      map[string]string{entryPlaylistName: entryURL},
		byURL:       map[string]string{entryURL: entryPlaylistName},
		variantName: map[string]string{},
		labelByURL:  map[string]string{},
	}
	return session
}

func (s *hlsSession) alive(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Before(s.expiresAt)
}

func (s *hlsSession) touch(expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expiresAt.After(s.expiresAt) {
		s.expiresAt = expiresAt
	}
}

func (s *hlsSession) deadline() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiresAt
}

func (s *hlsSession) lookup(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, found := s.byName[name]
	return target, found
}

func (s *hlsSession) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byName)
}

// registerVariant binds a human readable quality label such as "1080p" to an
// upstream media playlist so it can be served from /hls/{id}/1080p/index.m3u8.
func (s *hlsSession) registerVariant(variant Variant, rawURL string) (string, error) {
	name, err := s.nameFor(rawURL, kindIndex)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if label, found := s.labelByURL[rawURL]; found {
		return label, nil
	}
	if len(s.variants) >= maxVariantsPerSession {
		return "", fmt.Errorf("HLS session variant limit reached")
	}
	label := uniqueLabel(variant.Label, s.variantName)
	variant.Label = label
	variant.Path = label + "/" + variantPlaylistName
	s.variants = append(s.variants, variant)
	s.variantName[label] = name
	s.labelByURL[rawURL] = label
	return label, nil
}

func (s *hlsSession) labelFor(rawURL string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	label, found := s.labelByURL[rawURL]
	return label, found
}

// variantTarget resolves a quality label to the proxy resource name of its
// media playlist.
func (s *hlsSession) variantTarget(label string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, found := s.variantName[strings.ToLower(label)]
	if !found {
		name, found = s.variantName[label]
	}
	return name, found
}

// qualities returns the selectable levels, best quality first.
func (s *hlsSession) qualities() []Variant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Variant, len(s.variants))
	copy(out, s.variants)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Height != out[j].Height {
			return out[i].Height > out[j].Height
		}
		return out[i].Bandwidth > out[j].Bandwidth
	})
	return out
}

func uniqueLabel(base string, taken map[string]string) string {
	base = sanitizeLabel(base)
	if base == "" {
		base = "variant"
	}
	if _, clash := taken[base]; !clash {
		return base
	}
	for index := 2; ; index++ {
		candidate := base + "-" + strconv.Itoa(index)
		if _, clash := taken[candidate]; !clash {
			return candidate
		}
	}
}

var unsafeLabelChars = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeLabel(value string) string {
	value = unsafeLabelChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(value)), "-")
	value = strings.Trim(value, "-")
	if len(value) > 24 {
		value = strings.Trim(value[:24], "-")
	}
	return value
}

var (
	resolutionAttr = regexp.MustCompile(`(?i)RESOLUTION=(\d+)x(\d+)`)
	bandwidthAttr  = regexp.MustCompile(`(?i)[^-]BANDWIDTH=(\d+)`)
	nameAttr       = regexp.MustCompile(`(?i)NAME="([^"]+)"`)
)

// variantFromStreamInf derives a quality label from an #EXT-X-STREAM-INF tag,
// preferring the vertical resolution ("1080p") over bandwidth ("2500k").
func variantFromStreamInf(tag string) Variant {
	variant := Variant{}
	if match := resolutionAttr.FindStringSubmatch(tag); len(match) > 2 {
		width, _ := strconv.Atoi(match[1])
		height, _ := strconv.Atoi(match[2])
		variant.Height = height
		variant.Resolution = match[1] + "x" + match[2]
		if height > 0 {
			variant.Label = strconv.Itoa(height) + "p"
		} else if width > 0 {
			variant.Label = strconv.Itoa(width) + "w"
		}
	}
	if match := bandwidthAttr.FindStringSubmatch(" " + tag); len(match) > 1 {
		variant.Bandwidth, _ = strconv.Atoi(match[1])
	}
	if variant.Label == "" {
		if match := nameAttr.FindStringSubmatch(tag); len(match) > 1 {
			variant.Label = match[1]
		} else if variant.Bandwidth > 0 {
			variant.Label = strconv.Itoa(variant.Bandwidth/1000) + "k"
		}
	}
	return variant
}

// nameFor returns the stable proxy file name for an upstream URL, allocating a
// new one the first time the URL is seen inside this session.
func (s *hlsSession) nameFor(rawURL, kind string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name, found := s.byURL[rawURL]; found {
		return name, nil
	}
	if len(s.byName) >= maxResourcesPerSession {
		return "", fmt.Errorf("HLS session resource limit reached")
	}
	s.counter++
	name := fmt.Sprintf("%s-%d%s", kind, s.counter, resourceExtension(rawURL, kind))
	s.byName[name] = rawURL
	s.byURL[rawURL] = name
	return name, nil
}

type HLSProxy struct {
	client   *http.Client
	ttl      time.Duration
	mu       sync.RWMutex
	sessions map[string]*hlsSession
}

func NewHLSProxy(ttl time.Duration) *HLSProxy {
	return &HLSProxy{
		client:   &http.Client{Timeout: 90 * time.Second},
		ttl:      ttl,
		sessions: make(map[string]*hlsSession),
	}
}

func randomToken() (string, error) {
	data := make([]byte, 18)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func validUpstream(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}

var (
	safeExtension = regexp.MustCompile(`^\.[A-Za-z0-9]{1,5}$`)
	safeSessionID = regexp.MustCompile(`^[a-f0-9]{8,64}$`)
	safeName      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	safeLabel     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	playlistURI   = regexp.MustCompile(`URI=("[^"]+"|'[^']+')`)
)

func resourceExtension(rawURL, kind string) string {
	extension := ""
	if parsed, err := url.Parse(rawURL); err == nil {
		extension = strings.ToLower(path.Ext(parsed.Path))
	}
	if safeExtension.MatchString(extension) {
		return extension
	}
	switch kind {
	case kindIndex:
		return ".m3u8"
	case kindKey:
		return ".key"
	case kindInit:
		return ".mp4"
	default:
		return ".ts"
	}
}

func contentTypeForName(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".m3u8", ".m3u":
		return "application/vnd.apple.mpegurl"
	case ".ts":
		return "video/mp2t"
	case ".mp4", ".m4s", ".m4v":
		return "video/mp4"
	case ".m4a", ".aac":
		return "audio/aac"
	case ".vtt":
		return "text/vtt"
	}
	return "application/octet-stream"
}

// CreateSessionWithVariants registers a session whose entry playlist is a
// master playlist synthesized by the proxy, so that every quality level is
// exposed as /hls/{id}/{label}/index.m3u8 and players can switch between them.
// masterURL is only used to de-duplicate sessions; it is never fetched.
func (p *HLSProxy) CreateSessionWithVariants(masterURL, referer string, sources []VariantSource) (string, error) {
	usable := make([]VariantSource, 0, len(sources))
	for _, source := range sources {
		if validUpstream(source.URL) {
			usable = append(usable, source)
		}
	}
	if len(usable) == 0 {
		return "", fmt.Errorf("no playable HLS variants")
	}
	if masterURL == "" {
		masterURL = usable[0].URL
	}
	id, err := p.CreateSession(masterURL, referer)
	if err != nil {
		return "", err
	}
	session, found := p.session(id)
	if !found {
		return "", fmt.Errorf("HLS session disappeared while registering variants")
	}
	session.mu.Lock()
	alreadyBuilt := session.synthetic && len(session.variants) > 0
	session.synthetic = true
	session.mu.Unlock()
	if alreadyBuilt {
		return id, nil
	}
	for _, source := range usable {
		variant := Variant{
			Label:      source.Label,
			Resolution: source.Resolution,
			Bandwidth:  source.Bandwidth,
			Height:     source.Height,
		}
		if variant.Label == "" {
			variant.Label = source.Resolution
		}
		if _, err := session.registerVariant(variant, source.URL); err != nil {
			return "", err
		}
	}
	return id, nil
}

// Qualities lists the selectable quality levels of a session, best first.
func (p *HLSProxy) Qualities(id string) []Variant {
	session, found := p.session(id)
	if !found {
		return nil
	}
	return session.qualities()
}

// VariantPath is the client facing path of a single quality level.
func (p *HLSProxy) VariantPath(id, label string) string {
	return "/hls/" + id + "/" + label + "/" + variantPlaylistName
}

// masterPlaylist renders the synthetic master playlist of a session.
func (s *hlsSession) masterPlaylist() []byte {
	var output strings.Builder
	output.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for _, variant := range s.qualities() {
		bandwidth := variant.Bandwidth
		if bandwidth <= 0 {
			bandwidth = 1 << 20
			if variant.Height > 0 {
				bandwidth = variant.Height * 3000
			}
		}
		output.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d", bandwidth))
		if variant.Resolution != "" {
			output.WriteString(",RESOLUTION=" + variant.Resolution)
		}
		output.WriteString(",NAME=\"" + variant.Label + "\"\n")
		output.WriteString(variant.Path + "\n")
	}
	return []byte(output.String())
}

// CreateSession registers the entry playlist and returns the session ID used in
// /hls/{id}/playlist.m3u8.
func (p *HLSProxy) CreateSession(entryURL, referer string) (string, error) {
	if !validUpstream(entryURL) {
		return "", fmt.Errorf("invalid HLS upstream URL")
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked(now)
	for _, session := range p.sessions {
		if session.entryURL == entryURL && session.referer == referer {
			session.touch(now.Add(p.ttl))
			return session.id, nil
		}
	}
	p.evictLocked()
	id, err := randomToken()
	if err != nil {
		return "", err
	}
	p.sessions[id] = newHLSSession(id, entryURL, referer, now.Add(p.ttl))
	return id, nil
}

// PlaylistPath is the client facing entry point of a session.
func (p *HLSProxy) PlaylistPath(id string) string {
	return "/hls/" + id + "/" + entryPlaylistName
}

func (p *HLSProxy) sweepLocked(now time.Time) {
	for id, session := range p.sessions {
		if !session.alive(now) {
			delete(p.sessions, id)
		}
	}
}

func (p *HLSProxy) evictLocked() {
	for len(p.sessions) >= maxSessions {
		oldestID := ""
		var oldest time.Time
		for id, session := range p.sessions {
			deadline := session.deadline()
			if oldestID == "" || deadline.Before(oldest) {
				oldestID, oldest = id, deadline
			}
		}
		if oldestID == "" {
			return
		}
		delete(p.sessions, oldestID)
	}
}

func (p *HLSProxy) session(id string) (*hlsSession, bool) {
	p.mu.RLock()
	session, found := p.sessions[id]
	p.mu.RUnlock()
	if !found {
		return nil, false
	}
	if !session.alive(time.Now()) {
		p.mu.Lock()
		delete(p.sessions, id)
		p.mu.Unlock()
		return nil, false
	}
	return session, true
}

func resolveReference(base *url.URL, value string) (string, error) {
	reference, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return base.ResolveReference(reference).String(), nil
}

func attributeKind(tag string) string {
	switch {
	case strings.HasPrefix(tag, "#EXT-X-KEY"), strings.HasPrefix(tag, "#EXT-X-SESSION-KEY"):
		return kindKey
	case strings.HasPrefix(tag, "#EXT-X-MAP"):
		return kindInit
	case strings.HasPrefix(tag, "#EXT-X-PART"), strings.HasPrefix(tag, "#EXT-X-PRELOAD-HINT"):
		return kindSegment
	default:
		return kindIndex
	}
}

// rewritePlaylist replaces every upstream reference with a session relative
// file name, so the playlist stays valid while never leaking upstream URLs.
// qualityPrefix keeps references resolvable when a playlist is served from the
// nested /hls/{id}/{quality}/index.m3u8 path instead of /hls/{id}/.
func qualityPrefix(nested bool) string {
	if nested {
		return "../"
	}
	return ""
}

func (p *HLSProxy) rewritePlaylist(session *hlsSession, content []byte, sourceURL string) ([]byte, error) {
	return p.rewritePlaylistFor(session, content, sourceURL, false)
}

// rewritePlaylistFor rewrites a playlist; nested must be true when the result
// is served from /hls/{id}/{quality}/index.m3u8 so relative names still point
// back at /hls/{id}/.
func (p *HLSProxy) rewritePlaylistFor(session *hlsSession, content []byte, sourceURL string, nested bool) ([]byte, error) {
	sourceIsVariantPath := nested
	base, err := url.Parse(sourceURL)
	if err != nil {
		return nil, err
	}
	rewrite := func(value, kind string) (string, error) {
		resolved, err := resolveReference(base, value)
		if err != nil {
			return "", err
		}
		if strings.HasSuffix(strings.ToLower(strings.Split(resolved, "?")[0]), ".m3u8") {
			kind = kindIndex
		}
		return session.nameFor(resolved, kind)
	}

	var output strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	pendingKind := kindSegment
	var pendingVariant *Variant
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case !strings.HasPrefix(trimmed, "#"):
			if pendingVariant != nil {
				// Variant of a master playlist: expose it under a readable
				// quality path so the player can offer a quality picker.
				resolved, err := resolveReference(base, trimmed)
				if err != nil {
					return nil, err
				}
				label, err := session.registerVariant(*pendingVariant, resolved)
				if err != nil {
					return nil, err
				}
				pendingVariant = nil
				pendingKind = kindSegment
				line = qualityPrefix(sourceIsVariantPath) + label + "/" + variantPlaylistName
				break
			}
			name, err := rewrite(trimmed, pendingKind)
			if err != nil {
				return nil, err
			}
			pendingKind = kindSegment
			line = qualityPrefix(sourceIsVariantPath) + name
		default:
			tag := strings.ToUpper(trimmed)
			if strings.HasPrefix(tag, "#EXT-X-STREAM-INF") {
				pendingKind = kindIndex
				variant := variantFromStreamInf(trimmed)
				pendingVariant = &variant
			}
			if strings.Contains(line, "URI=") {
				kind := attributeKind(tag)
				var replacementErr error
				line = playlistURI.ReplaceAllStringFunc(line, func(match string) string {
					if replacementErr != nil {
						return match
					}
					quoted := strings.TrimPrefix(match, "URI=")
					if len(quoted) < 2 {
						return match
					}
					name, err := rewrite(quoted[1:len(quoted)-1], kind)
					if err != nil {
						replacementErr = err
						return match
					}
					return `URI="` + qualityPrefix(sourceIsVariantPath) + name + `"`
				})
				if replacementErr != nil {
					return nil, replacementErr
				}
			}
		}
		output.WriteString(line)
		output.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return []byte(output.String()), nil
}

func copyProxyHeaders(destination, source http.Header) {
	for _, name := range []string{"Accept-Ranges", "Content-Range", "ETag", "Last-Modified"} {
		if value := source.Get(name); value != "" {
			destination.Set(name, value)
		}
	}
}

func setCORSHeaders(header http.Header) {
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	header.Set("Access-Control-Allow-Headers", "Range, Origin, Accept")
	header.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
	header.Set("Access-Control-Max-Age", "600")
}

func isPlaylist(contentType, rawURL string, prefix []byte) bool {
	return strings.Contains(strings.ToLower(contentType), "mpegurl") ||
		strings.HasSuffix(strings.ToLower(strings.Split(rawURL, "?")[0]), ".m3u8") ||
		strings.HasPrefix(strings.TrimSpace(string(prefix)), "#EXTM3U")
}

// splitResourcePath splits /hls/{id}/{name} or /hls/{id}/{quality}/index.m3u8
// into its components. quality is empty for flat resource paths.
func splitResourcePath(requestPath string) (id, quality, name string, ok bool) {
	rest := strings.TrimPrefix(requestPath, "/hls/")
	id, rest, found := strings.Cut(rest, "/")
	if !found || !safeSessionID.MatchString(id) {
		return "", "", "", false
	}
	if first, second, nested := strings.Cut(rest, "/"); nested {
		if !safeLabel.MatchString(first) || !safeName.MatchString(second) {
			return "", "", "", false
		}
		return id, first, second, true
	}
	if !safeName.MatchString(rest) {
		return "", "", "", false
	}
	return id, "", rest, true
}

func (p *HLSProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w.Header())
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	id, quality, name, ok := splitResourcePath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("HLS resource not found"))
		return
	}
	session, found := p.session(id)
	if !found {
		writeError(w, http.StatusGone, fmt.Errorf("HLS session expired or was not found"))
		return
	}
	session.touch(time.Now().Add(p.ttl))

	nested := false
	switch {
	case quality != "":
		// /hls/{id}/{quality}/index.m3u8 — a specific quality level.
		if name != variantPlaylistName {
			writeError(w, http.StatusNotFound, fmt.Errorf("only %s is served under a quality path", variantPlaylistName))
			return
		}
		if strings.EqualFold(quality, autoQuality) {
			p.serveEntryPlaylist(w, r, session, true)
			return
		}
		resolved, found := session.variantTarget(quality)
		if !found {
			writeError(w, http.StatusNotFound, fmt.Errorf("quality %q is not available in this session", quality))
			return
		}
		name, nested = resolved, true
	case name == qualityListName:
		writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "qualities": session.qualities()})
		return
	case name == entryPlaylistName:
		p.serveEntryPlaylist(w, r, session, false)
		return
	}

	target, found := session.lookup(name)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("HLS resource not found in this session"))
		return
	}
	p.proxyResource(w, r, session, name, target, nested)
}

// serveEntryPlaylist answers /hls/{id}/playlist.m3u8 (and the "auto" quality
// path). When the session was created from a list of variants the master
// playlist is synthesized locally so every quality stays selectable.
func (p *HLSProxy) serveEntryPlaylist(w http.ResponseWriter, r *http.Request, session *hlsSession, nested bool) {
	session.mu.Lock()
	synthetic := session.synthetic
	session.mu.Unlock()
	if !synthetic {
		p.proxyResource(w, r, session, entryPlaylistName, session.entryURL, nested)
		return
	}
	playlist := session.masterPlaylist()
	if nested {
		var rebuilt strings.Builder
		for _, line := range strings.Split(strings.TrimSuffix(string(playlist), "\n"), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				line = "../" + line
			}
			rebuilt.WriteString(line)
			rebuilt.WriteByte('\n')
		}
		playlist = []byte(rebuilt.String())
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(playlist)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(playlist)
	}
}

// proxyResource streams one upstream resource, rewriting it first when it turns
// out to be a playlist.
func (p *HLSProxy) proxyResource(w http.ResponseWriter, r *http.Request, session *hlsSession, name, target string, nested bool) {
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	request.Header.Set("User-Agent", userAgent)
	if session.referer != "" {
		request.Header.Set("Referer", session.referer)
	}
	if value := r.Header.Get("Range"); value != "" {
		request.Header.Set("Range", value)
	}
	response, err := p.client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer response.Body.Close()

	copyProxyHeaders(w.Header(), response.Header)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		return
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = contentTypeForName(name)
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", contentType)
		if length := response.Header.Get("Content-Length"); length != "" {
			w.Header().Set("Content-Length", length)
		}
		w.WriteHeader(response.StatusCode)
		return
	}
	prefix := make([]byte, 16)
	count, readErr := io.ReadFull(response.Body, prefix)
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		writeError(w, http.StatusBadGateway, readErr)
		return
	}
	prefix = prefix[:count]
	if isPlaylist(contentType, target, prefix) {
		const maxPlaylistSize = 4 << 20
		content, err := io.ReadAll(io.LimitReader(io.MultiReader(strings.NewReader(string(prefix)), response.Body), maxPlaylistSize+1))
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		if len(content) > maxPlaylistSize {
			writeError(w, http.StatusBadGateway, fmt.Errorf("upstream HLS playlist is too large"))
			return
		}
		rewritten, err := p.rewritePlaylistFor(session, content, target, nested)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		w.Header().Del("Content-Range")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", fmt.Sprint(len(rewritten)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rewritten)
		return
	}
	w.Header().Set("Content-Type", contentType)
	if length := response.Header.Get("Content-Length"); length != "" {
		w.Header().Set("Content-Length", length)
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(prefix)
	_, _ = io.Copy(w, response.Body)
}
