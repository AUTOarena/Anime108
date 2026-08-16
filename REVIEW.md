# รายงานตรวจสอบโค้ด — Anime108 HLS Proxy

ตรวจเมื่อ 2026-08-16 · branch `arena/01a00c0a-anime108`
หมายเหตุ: sandbox ไม่มี Go toolchain และเข้าเน็ตไม่ได้ จึงตรวจด้วยการอ่านโค้ดและไล่ import/สัญลักษณ์เองทั้งหมด (ยังไม่ได้ `go build` / `go test` จริง)

---

## 1. ระดับวิกฤต — build ไม่ผ่าน

| # | ไฟล์ | ปัญหา | สถานะ |
|---|------|-------|-------|
| 1 | `scraper.go:256` | ใช้ `errors.New(...)` แต่ไม่ได้ `import "errors"` → `undefined: errors` คอมไพล์ไม่ผ่านทั้ง repo (ทั้ง `go build`, `go vet`, `go test`) | **แก้แล้ว** (เพิ่ม `"errors"` ใน import block) |

หลังแก้ข้อนี้ ผมไล่ import ของทุกไฟล์ (`main.go`, `scraper.go`, `hls_proxy.go`, ไฟล์เทสต์ 2 ไฟล์) แล้ว ไม่พบ import ที่ไม่ได้ใช้หรือสัญลักษณ์ที่ยังไม่ประกาศเพิ่มเติม

---

## 2. ระดับสูง — ความเสี่ยงตอนรันจริง

1. **ไม่มี timeout ของ `http.Server`** — `main.go` ใช้ `http.ListenAndServe` ตรง ๆ ไม่มี `ReadHeaderTimeout` / `IdleTimeout` (gosec G114) เสี่ยง Slowloris
   *ควรมี:* `&http.Server{Addr: ..., Handler: ..., ReadHeaderTimeout: 10s, IdleTimeout: 120s}` (อย่าตั้ง `WriteTimeout` เพราะ stream ยาว)
2. **ไม่มี graceful shutdown** — ไม่ดัก SIGINT/SIGTERM, คอนเทนเนอร์ถูก kill กลางสตรีมทันที
3. **จำนวน token ไม่มีเพดาน (memory DoS)** — `rewritePlaylist` ลงทะเบียน token ให้ทุกบรรทัด segment ของ playlist; live playlist ที่รีเฟรชบ่อยจะสะสม entry ใน `resources`/`byTarget` เรื่อย ๆ เพราะกวาดของหมดอายุเฉพาะตอน `Register` และไม่มีเพดานจำนวน
   *ควรมี:* max entries + goroutine janitor กวาดตาม interval
4. **`templates/*.html` ผูกกับ current working directory** — `template.ParseGlob("templates/*.html")` พังทันทีถ้ารันไบนารีจากไดเรกทอรีอื่น
   *ควรมี:* `//go:embed templates/*.html` + `template.ParseFS` (แล้วจะตัด `COPY templates` ใน Dockerfile ได้ด้วย)
5. **ไม่มี `/healthz` / readiness endpoint** — Docker/K8s เช็คสถานะไม่ได้
6. **ไม่มี HEAD/OPTIONS + CORS preflight ที่ `/hls/`** — ตั้ง `Access-Control-Allow-Origin: *` แต่ไม่ตอบ `OPTIONS` และไม่ส่ง `Access-Control-Allow-Headers: Range` / `Expose-Headers: Content-Range` → ถ้าเล่นข้ามโดเมนด้วย hls.js จะติด
7. **ไม่มี rate limit / concurrency limit** บน `/search`, `/api/parse`, `/api/stream` ซึ่งทุกตัวยิงออก upstream ต่อ 1 request

---

## 3. ระดับกลาง — งานที่หายไปในโปรเจกต์

1. **ไม่มี CI** — ไม่มี `.github/workflows/` เลย (ควรมี `go vet` + `go test -race` + `gofmt -l` บน push/PR) ข้อบกพร่องข้อ 1 หลุดมาได้เพราะไม่มี CI
2. **ไม่มี `LICENSE`** ทั้งที่ README เป็น public-facing
3. **ไม่มี `.dockerignore`** — `.git` ถูกส่งเข้า build context
4. **Dockerfile เปราะ** — `COPY main.go scraper.go hls_proxy.go ./` ระบุไฟล์ทีละชื่อ เพิ่มไฟล์ `.go` ใหม่แล้วลืมแก้ = build พังเงียบ ๆ ควรเป็น `COPY . .` (คู่กับ `.dockerignore`) และเพิ่ม `HEALTHCHECK`, non-root user
5. **Go version ไม่ตรงกัน** — `go.mod` = `go 1.22`, README = "Go 1.22+", Dockerfile ใช้ `golang:1.24-alpine`
6. **module path ไม่ตรง repo** — `module github.com/SIX460/Anime108` แต่ repo คือ `AUTOarena/Anime108`
7. **`.gitignore` มีแค่ `/anime108`** — ขาด `*.exe`, `coverage.out`, `*.test`, `.DS_Store`, `/tmp`
8. **ไม่มีการตั้งค่าแบบ env var** — port/TTL/timeout ฮาร์ดโค้ด (`2 * time.Hour` อยู่ใน `NewServer`) ควรอ่านจาก `PORT`, `HLS_TTL`
9. **ไม่มี access log / request ID** — มีแต่ `fmt.Printf("Fetching URL: %s\n", ...)` ใน `scraper.go` ซึ่งควรเป็น `log` และควรมี middleware logging กลาง

---

## 4. ช่องว่างของเทสต์

ที่มีอยู่ (5 เคส) ครอบคลุมเฉพาะ `rewritePlaylist`, proxy playlist+segment, token หมดอายุ, `ParseShowPage` (+fallback), `balancedDivBlocks`

**ที่ยังไม่มีเลย:**
- `main.go` ทั้งไฟล์ — ไม่มีเทสต์ handler สักตัว: `decodeRequest` (validate host ต้องเป็น anime108.com, lang ต้องเป็น `Thai`/`Sound Track`, JSON เสีย, body เกิน 1MB), `requireMethod` (405 + header `Allow`), `/search` ที่ไม่มี `q`/`keyword` (400), routing ของ `Handler()`
- `SearchAnime` — parsing ผลค้นหาจาก HTML (ใช้ `httptest` mock ได้)
- `GetPlayerIframe` — เคส `//host/...` → เติม `https:`, เคสหา iframe ไม่เจอ
- `ResolveStreamURL` — fallback `newplaylist` → `newplaylist_g`, การเรียงเลือกความละเอียดสูงสุด, fallback `m3u8_g` → `m3u8`
- HLS proxy: การส่งต่อ **Range request** (`Content-Range`/`Accept-Ranges`) ทั้งที่ README โฆษณาไว้, `HEAD`, การส่งต่อ status code ที่ไม่ใช่ 2xx, playlist ใหญ่เกิน 4MB, token ที่มี `/` → 404
- `Register` ซ้ำ target เดิม → ต้องได้ token เดิมและต่ออายุ
- ยังไม่มีการวัด coverage

---

## 5. เอกสาร

- `api_docs.md` / `templates/docs.html` ระบุเฉพาะ `?q=` แต่โค้ดรับ `?keyword=` ด้วย — ไม่ได้เขียนไว้
- ไม่ได้ระบุ error response schema (`{"error": "..."}`) และตารางรหัสสถานะ (400 / 405 / 410 / 500 / 502)
- ไม่มีข้อความ disclaimer เรื่องลิขสิทธิ์/การใช้งาน ทั้งที่เป็น proxy ดึงเนื้อหาจากเว็บบุคคลที่สาม
- README ไม่มีหัวข้อ Configuration และไม่บอกว่า `templates/` ต้องอยู่ใน CWD

---

## ลำดับที่แนะนำให้ทำต่อ

1. ~~เพิ่ม `import "errors"`~~ (ทำแล้ว) → รัน `go build ./...` และ `go test -race ./...` ยืนยัน
2. เพิ่ม GitHub Actions CI (vet + test + gofmt) กันปัญหาแบบข้อ 1 ซ้ำ
3. `//go:embed` เทมเพลต + `http.Server` ที่มี timeout + graceful shutdown + `/healthz`
4. ใส่เพดานจำนวน token และ janitor กวาดของหมดอายุ
5. เติมเทสต์ handler ใน `main_test.go` และเทสต์ Range ของ proxy
6. เก็บงานเล็ก: `.dockerignore`, `LICENSE`, Dockerfile `COPY . .`, จูน go version / module path ให้ตรงกัน
