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
  "playlist_url": "/hls/temporary-token",
  "title": "Mushen Ji",
  "episode": 2,
  "lang": "Sound Track",
  "expires_in": 7200
}
```

## Stream a proxied HLS resource

```http
GET /hls/{token}
Range: bytes=0-1048575
```

`playlist_url` และ URI ที่ถูก rewrite ภายใน playlist ใช้ endpoint นี้ทั้งหมด Proxy รองรับ:

- Master และ media playlists
- MPEG-TS/fMP4 segments
- `EXT-X-KEY` และ URI attributes
- Relative และ absolute upstream URLs
- HTTP Range requests

Token เป็นค่า opaque, ไม่เปิดเผย upstream URL และมีอายุ 2 ชั่วโมง เมื่อหมดอายุจะตอบ `410 Gone`
