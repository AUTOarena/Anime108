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
  "playlist_url": "/hls/temporary-token/master.m3u8",
  "title": "Mushen Ji",
  "episode": 2,
  "lang": "Sound Track",
  "expires_in": 7200
}
```

## Stream a proxied HLS resource

```http
GET /hls/{id}/master.m3u8
GET /hls/{id}/{quality}/index.m3u8
Range: bytes=0-1048575
```

`playlist_url` เป็น master playlist ที่เก็บทุกความชัดไว้ เช่น URI ภายในจะถูก rewrite เป็น `/hls/{id}/720p/index.m3u8` และ `/hls/{id}/1080p/index.m3u8` ทำให้ HLS player เลือกหรือสลับความชัดได้ ส่วน URI ของ segment/key จะใช้ opaque token ผ่าน `/hls/{token}` โดยอัตโนมัติ Proxy รองรับ:

- Master playlist แบบ adaptive bitrate และ media playlists
- MPEG-TS/fMP4 segments
- `EXT-X-KEY` และ URI attributes
- Relative และ absolute upstream URLs
- HTTP Range requests

Token เป็นค่า opaque, ไม่เปิดเผย upstream URL และมีอายุ 2 ชั่วโมง เมื่อหมดอายุจะตอบ `410 Gone`
