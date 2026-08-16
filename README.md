# Anime108 Downloader (Go)

Anime108 scraper, stream resolver และตัวดาวน์โหลดวิดีโอที่เขียนด้วย Go ล้วน พร้อม Web UI, JSON API และ CLI ในไบนารีเดียว

## Requirements

- Go 1.22 ขึ้นไป
- FFmpeg (แนะนำ เพื่อ remux MPEG-TS เป็น MP4 อย่างถูกต้อง)

## เริ่ม Web UI

```bash
go run .
```

จากนั้นเปิด <http://localhost:5000>

กำหนดพอร์ตหรือโฟลเดอร์ดาวน์โหลดได้:

```bash
go run . -port 8080 -dir ./downloads
```

## ใช้งานผ่าน CLI

```bash
go run . \
  -url "https://www.anime108.com/mushen-ji-ep-2/" \
  -lang "Sound Track" \
  -threads 16 \
  -dir ./downloads
```

ตรวจสอบว่า playlist ใช้งานได้โดยไม่ดาวน์โหลด:

```bash
go run . -url "https://www.anime108.com/mushen-ji-ep-2/" -check-only
```

`-lang` รองรับ `Sound Track` (ซับไทย) และ `Thai` (พากย์ไทย)

## Build

```bash
go build -o anime108 .
./anime108
```

## Test

```bash
go test ./...
go test -race ./...
```

## API

เมื่อ server ทำงาน สามารถเปิดเอกสารแบบ interactive ที่ <http://localhost:5000/docs> หรืออ่าน [api_docs.md](api_docs.md)
