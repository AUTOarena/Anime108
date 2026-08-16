# Anime108 HLS Proxy (Go)

HLS streaming proxy สำหรับ Anime108 เขียนด้วย Go พร้อม Web player และ JSON API โดย **ไม่มีระบบดาวน์โหลดและไม่บันทึกไฟล์วิดีโอลงดิสก์**

## ความสามารถ

- ค้นหาเรื่องและอ่านรายการตอน
- Resolve upstream HLS stream
- Proxy playlist, video segments, encryption keys และ HLS resources
- Rewrite URI ภายใน M3U8 ให้ผ่าน proxy อัตโนมัติ
- ใช้ opaque token จึงไม่เปิดเผย upstream URL กับ client
- Session หมดอายุอัตโนมัติภายใน 2 ชั่วโมง
- รองรับ HTTP Range สำหรับ video segments

## เริ่มใช้งาน

ต้องใช้ Go 1.22 ขึ้นไป:

```bash
go run .
```

เปิด <http://localhost:5000>

กำหนดพอร์ต:

```bash
go run . -port 8080
```

## Build

```bash
go build -o anime108 .
./anime108
```

## Docker

```bash
docker build -t anime108-hls .
docker run --rm -p 5000:5000 anime108-hls
```

## Test

```bash
go test ./...
go test -race ./...
```

## API

- `GET /search?q=...` — ค้นหาอนิเมะ
- `POST /api/parse` — อ่าน metadata และรายการตอน
- `POST /api/stream` — สร้าง HLS proxy session
- `GET /hls/{token}` — รับ playlist/segment/key ผ่าน proxy

ดูรายละเอียดที่ <http://localhost:5000/docs> หรือ [api_docs.md](api_docs.md)
