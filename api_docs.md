# Anime108 HLS Proxy API

Default Base URL: `http://localhost:5000`

ระบบนี้ส่ง HLS แบบ streaming ผ่าน proxy และไม่บันทึกไฟล์วิดีโอลงดิสก์

## Search

```http
GET /search?q=mushen
```

คืนรายการเรื่องที่ค้นพบจาก Anime108

## Parse show

```http
POST /api/parse
Content-Type: application/json

{"url":"https://www.anime108.com/mushen-ji/"}
```

คืน title, post ID, current episode และรายการตอนแยก `Thai`/`Sound Track`

## Create HLS proxy session

```http
POST /api/stream
Content-Type: application/json

{
  "url": "https://www.anime108.com/mushen-ji-ep-2/",
  "lang": "Sound Track"
}
```

`lang` รองรับ `Sound Track` และ `Thai`

Response:

```json
{
  "playlist_url": "/hls/8f2c1a9e4b7d6350/playlist.m3u8",
  "session_id": "8f2c1a9e4b7d6350",
  "title": "Mushen Ji",
  "episode": 2,
  "lang": "Sound Track",
  "expires_in": 7200
}
```

## Stream a proxied HLS resource

ทุก resource ของ session อยู่ใต้ `/hls/{id}/` และมีนามสกุลไฟล์จริง ทำให้ player,
CDN และ `Content-Type` ทำงานได้ถูกต้อง

```http
GET /hls/{id}/playlist.m3u8      # entry playlist
GET /hls/{id}/index-1.m3u8       # media playlist ของแต่ละ variant
GET /hls/{id}/segment-12.ts      # video segment
GET /hls/{id}/key-3.key          # AES-128 key
GET /hls/{id}/init-2.mp4         # fMP4 initialization section
Range: bytes=0-1048575
```

Server จะ rewrite ทุก URI ภายใน playlist ให้เป็นชื่อไฟล์แบบ relative ของ session เดียวกัน
เช่น `segment-12.ts` ดังนั้น player จะ resolve เป็น `/hls/{id}/segment-12.ts` เองอัตโนมัติ

Proxy รองรับ:

- Master และ media playlists (`#EXT-X-STREAM-INF`)
- MPEG-TS และ fMP4 segments
- `EXT-X-KEY`, `EXT-X-SESSION-KEY`, `EXT-X-MAP`, `EXT-X-PART` และ URI attributes อื่น
- Relative และ absolute upstream URLs
- HTTP Range requests (ตอบ `206` พร้อม `Content-Range`)
- CORS preflight (`OPTIONS`) และ `HEAD`
- Live playlist: reload แล้วชื่อไฟล์เดิมคงที่ ไม่สร้าง entry ซ้ำ

| Status | ความหมาย |
| --- | --- |
| `200` / `206` | สำเร็จ (206 เมื่อมี Range) |
| `404` | ไม่พบ resource นี้ใน session (หรือ path ผิดรูปแบบ) |
| `405` | method ไม่รองรับ (มี header `Allow`) |
| `410` | session หมดอายุหรือไม่มีอยู่ |
| `502` | upstream ผิดพลาด |

`id` เป็นค่า opaque แบบสุ่ม ไม่เปิดเผย upstream URL และมีอายุ 2 ชั่วโมงนับจากการใช้งานครั้งล่าสุด
(ทุก request จะต่ออายุให้อัตโนมัติ) เมื่อหมดอายุจะตอบ `410 Gone`

## Error response

ทุก endpoint ตอบ error ในรูปแบบเดียวกัน:

```json
{"error": "HLS session expired or was not found"}
```
