# Order Tracking API

API ติดตามออเดอร์ร้านเติมเกม (ลูกค้าสั่ง → bot จ่ายเงินและเติมเกมอัตโนมัติ)
ระบบบอกสถานะออเดอร์ และตรวจเจอ/แก้เองเมื่อ bot ล่มหรือมีปัญหา

**Stack:** Go (net/http) + PostgreSQL + Docker Compose

## วิธีรัน

```
docker compose up --build
curl localhost:8080/health
```

- ทดสอบ API: เปิด `requests.http` ใน VS Code (ส่วนเสริม REST Client) แล้วกด Send Request ทีละอัน กดสร้างออเดอร์ (`createOrder`) ก่อนเสมอ
- ดู alert ที่แอดมินจะเห็น: `docker compose exec db psql -U app -d orders -c "SELECT type, order_no, message FROM alerts ORDER BY id DESC LIMIT 10;"`
- ค่าตั้งต่างๆ (lease, backoff, secret) อยู่ใน `docker-compose.yml` ค่าใน compose เป็นค่า dev สำหรับ demo

## ทดสอบเร็วด้วย DEV_MODE
REST Client เซ็น HMAC เองไม่ได้ จึงมีทางลัด `/dev/pay` และ `/dev/expire` ใช้ได้เมื่อ `DEV_MODE=true` ใน compose
ให้กดจบ flow (สร้าง → จ่าย → bot claim → ผลเติม) ได้ใน `requests.http` โดยไม่ต้องเซ็นเอง
⚠️ `/dev/*` ข้ามการตรวจลายเซ็นจริง ห้ามเปิดใน production

## Endpoints

| Method | Path | ใครเรียก | หน้าที่ |
|---|---|---|---|
| POST | /orders | ลูกค้า | สร้างออเดอร์ (ต้องมี header `Idempotency-Key`) |
| GET | /orders/:orderNo | ลูกค้า | ดูออเดอร์ (400 ถ้า format ผิด, 404 ถ้าไม่พบ) |
| GET | /orders?tab=current\|history&page= | ลูกค้า | รายการแบ่งหน้า ใหม่ → เก่า |
| GET | /orders/:orderNo/timeline | ลูกค้า | ประวัติการเปลี่ยนสถานะ + เหตุผล |
| POST | /orders/:orderNo/report-issue | ลูกค้า | แจ้งปัญหา (บันทึกเป็น alert `CUSTOMER_ISSUE`) |
| POST | /orders/:orderNo/fix-uid | ลูกค้า | แก้ UID แล้วลองใหม่ (`FAILED → QUEUED`) |
| POST | /orders/:orderNo/refund | ลูกค้า/แอดมิน | ขอคืนเงิน (`FAILED/DELAYED → REFUNDED`) |
| POST | /webhooks/bank | ธนาคาร | แจ้งเงินเข้า (ตรวจ header `X-Signature`) |
| POST | /bot/claim | bot | ขอรับงานถัดไป |
| POST | /bot/orders/:orderNo/heartbeat | bot | ต่ออายุ lease |
| POST | /bot/orders/:orderNo/payment | bot | บันทึกการจ่ายเงิน |
| POST | /bot/orders/:orderNo/result | bot | รายงานผลเติม (`SUCCESS`/`TRANSIENT`/`PERMANENT`) |
| GET | /health | - | เช็กว่าระบบทำงาน |
| POST | /dev/pay/:orderNo | ทดสอบ (DEV_MODE) | จำลองธนาคารแจ้งเงินเข้า ใช้ logic เดียวกับ /webhooks/bank |
| POST | /dev/expire/:orderNo | ทดสอบ (DEV_MODE) | ทำให้ออเดอร์ PENDING_PAYMENT หมดอายุ |

ทุก error ใช้รูปแบบเดียว: `{"error":{"code":"...","message":"..."}}`

## Design decisions

### Schema
- `orders` เก็บสถานะปัจจุบัน, `order_events` เก็บประวัติทุกการเปลี่ยนสถานะ (ใช้ทำ timeline)
- `payments` (unique ต่อออเดอร์), `webhook_events` (event id ที่ประมวลผลแล้ว), `alerts` (เรื่องที่แอดมินต้องดู)
- ให้ DB กันข้อมูลผิด: `UNIQUE` (order_no, idempotency_key), `CHECK` (รูปแบบ order_no, status, amount > 0)
- เงินใช้ `NUMERIC` ไม่ใช้ float

### Idempotency (POST /orders)
`INSERT ... ON CONFLICT (idempotency_key) DO NOTHING` แล้วคืนออเดอร์เดิมถ้าซ้ำ (201 ครั้งแรก, 200 ครั้งถัดไป)
เหตุผล: ให้ DB ตัดสินเอง ปลอดภัยกว่า "เช็กก่อนแล้ว insert" ที่เกิด race เมื่อยิงพร้อมกัน

### State machine

```
PENDING_PAYMENT → QUEUED → IN_PROGRESS → COMPLETED
PENDING_PAYMENT → EXPIRED → REFUNDED
IN_PROGRESS → QUEUED   (lease หมดหรือ error ชั่วคราว: retry)
IN_PROGRESS → DELAYED  (retry ครบ 3 ครั้งแล้วยังไม่ผ่าน)
IN_PROGRESS → FAILED   (bot รายงาน error ถาวร เช่น UID ผิด)
QUEUED → DELAYED
FAILED → QUEUED        (ลูกค้าแก้ UID แล้วลองใหม่)
FAILED → REFUNDED
DELAYED → QUEUED / FAILED / REFUNDED
```

- กฎทั้งหมดอยู่ที่เดียว (`state.go`) และมี unit test
- `COMPLETED` และ `REFUNDED` เป็นสถานะสุดท้าย
- `EXPIRED → REFUNDED` ใช้เมื่อลูกค้าจ่ายมาช้าหลังหมดอายุ

## เคสปัญหา 5 ข้อ

### เคส 1: Bot ล่ม / ค้าง
ใช้ **lease + heartbeat**: ตอน claim ได้ lease (`lease_expires_at`) bot ต้องส่ง heartbeat ต่ออายุ
reaper (goroutine รันทุก 5 วินาที) กวาดออเดอร์ที่ lease หมด:
- คืนเข้าคิวแบบ exponential backoff: หน่วง `BACKOFF_BASE × 2^attempts` วินาที ไม่เกิน 3 ครั้ง
- ถ้ายังไม่ผ่าน → `DELAYED` + alert `BOT_UNRESPONSIVE`

ค่าเวลา: ค่า default ในโค้ดคือ base 10 วินาที (10s, 20s, 40s) และ lease 15 วินาที ส่วน `docker-compose.yml` ตั้ง base 5 วินาที (5s, 10s, 20s) เพื่อให้ demo เร็ว

เหตุผล: ไม่ต้องเดาว่า bot ตายจริงหรือไม่ แค่ดูว่าเงียบเกินเวลา และ bot เก่าที่ฟื้นมาทีหลังจะต่อ lease/ส่งผลไม่ได้ (409 `LEASE_LOST`)

ทดสอบ: claim แล้วปล่อยเงียบ 4 รอบ → log เห็น retry 1/3, 2/3, 3/3 แล้ว ALERT → `DELAYED` (ตาราง `alerts` มี `BOT_UNRESPONSIVE`, timeline เห็นทุกรอบ)

### เคส 2: Bot สองตัวแย่งออเดอร์
- ด่านที่ 1: claim แบบ atomic ใน SQL เดียว `SELECT ... FOR UPDATE SKIP LOCKED` + `UPDATE` bot ที่มาทีหลังจะข้ามแถวที่ถูกล็อก
- ด่านที่ 2: ตาราง `payments` มี `UNIQUE(order_no)` ต่อให้โค้ดพลาด DB ก็ไม่ยอมให้จ่ายซ้ำ (409 `DUPLICATE_PAYMENT`)
- ทดสอบ: ยิง 10 bot พร้อมกันต่อ 1 ออเดอร์ → ชนะ 1 ตัวเสมอ ที่เหลือได้ 204
- ทางเลือกที่ไม่เลือก: optimistic lock (version) ต้องคอย retry เมื่อชน

### เคส 3: Webhook ธนาคารซ้ำ / ช้า / ผิดลำดับ
`POST /webhooks/bank` body: `{"eventId","orderNo","type":"payment.succeeded"}`

ด่านที่ผ่านตามลำดับ:
1. **ตรวจลายเซ็น:** HMAC-SHA256 ของ body ดิบ (header `X-Signature`, hex, secret จาก env `WEBHOOK_SECRET`) เทียบด้วย `hmac.Equal` ผิด → 401 `INVALID_SIGNATURE`
2. **กัน event ซ้ำ:** `webhook_events.event_id` เป็น PK + `ON CONFLICT DO NOTHING` ซ้ำ → 200 `duplicate`
3. **เทียบสถานะ:** `SELECT ... FOR UPDATE` แล้วตัดสินตามสถานะปัจจุบัน (เช็ก `CanTransition` ก่อนเปลี่ยนเสมอ)

| สถานะตอน webhook มา | ผล |
|---|---|
| `PENDING_PAYMENT` | → `QUEUED` |
| `EXPIRED` (มาช้าหลังหมดอายุ) | → `REFUNDED` + alert `REFUND_REQUIRED` |
| สถานะอื่น (จ่ายซ้ำ/ผิดจังหวะ) | ไม่แตะสถานะ + alert `UNEXPECTED_PAYMENT` |

ทั้งหมดอยู่ใน transaction เดียว ถ้าพังกลางทางจะ rollback ธนาคารจึงส่งซ้ำได้อย่างปลอดภัย
เหตุผล: ลายเซ็นกันคนปลอม, event id กันซ้ำ, การล็อกแถว + เทียบสถานะกัน webhook ที่ช้า/ผิดลำดับทำสถานะย้อนกลับ, เงินเข้าหลังหมดอายุต้องคืนเงินไม่ใช่เงียบ

**ทดสอบ webhook (PowerShell):** REST Client คำนวณ HMAC เองไม่ได้ จึงใช้ฟังก์ชันนี้เซ็นและส่ง

```powershell
function Send-Webhook($body, $secret = "dev-secret-change-me", $sig = $null) {
  if (-not $sig) {
    $h = New-Object System.Security.Cryptography.HMACSHA256
    $h.Key = [Text.Encoding]::UTF8.GetBytes($secret)
    $sig = -join ($h.ComputeHash([Text.Encoding]::UTF8.GetBytes($body)) | % { $_.ToString("x2") })
  }
  try {
    Invoke-RestMethod -Method Post -Uri http://localhost:8080/webhooks/bank `
      -Headers @{ "X-Signature" = $sig } -ContentType "application/json" -Body $body | ConvertTo-Json -Compress
  } catch { $_.ErrorDetails.Message }
}

# ใส่ orderNo จริงจากการสร้างออเดอร์
Send-Webhook '{"eventId":"evt-1","orderNo":"ORD-xxxxxxxxxxxxx-xxx","type":"payment.succeeded"}'
```

ผลที่ควรเห็น: ครั้งแรก `QUEUED` → ส่ง `evt-1` ซ้ำได้ `duplicate` → ส่ง `evt-2` ตอนเป็น `QUEUED` แล้วได้ `ignored` + alert → ใส่ `-sig "abcd"` ได้ `INVALID_SIGNATURE` → ออเดอร์ที่หมดอายุแล้วได้ `REFUNDED`

### เคส 4: เติมไม่สำเร็จ
bot รายงานผลที่ `POST /bot/orders/:orderNo/result` body `{"botId","outcome","reason"}`

| outcome | ผล |
|---|---|
| `SUCCESS` | → `COMPLETED` |
| `TRANSIENT` (เช่น เกมล่มชั่วคราว) | คืน `QUEUED` + backoff สูตรเดียวกับ reaper จนครบ 3 ครั้งแล้ว `DELAYED` + alert |
| `PERMANENT` (เช่น UID ผิด) | → `FAILED` ทันที ไม่เสียรอบ retry เหตุผลเก็บใน timeline |

ตรวจ lease ด้วย `FOR UPDATE` + `claimed_by`: bot ที่ lease หมดแล้วส่งผลมาได้ 409 `LEASE_LOST`

เมื่อออเดอร์เป็น `FAILED`:
- `POST /orders/:orderNo/fix-uid` body `{"gameUid"}` → `QUEUED` และรีเซ็ต `attempts`
- `POST /orders/:orderNo/refund` body `{"reason"}` → `REFUNDED` + alert `REFUND_REQUIRED` ให้แอดมินโอนคืนจริง

ทั้งสองล็อกแถวและเช็ก `CanTransition` สถานะไม่ตรงตอบ 409 `INVALID_STATE`
`POST /orders/:orderNo/report-issue` ลูกค้าแจ้งปัญหา บันทึกเป็น alert ไม่เปลี่ยนสถานะ ให้แอดมินตัดสิน

เหตุผล:
- แยก transient/permanent เพราะ UID ผิดลองกี่ครั้งก็ไม่ผ่าน ไม่ควรเสียรอบ retry และลูกค้าเห็นสาเหตุทันที
- refund ไม่รวม `EXPIRED` เพราะออเดอร์ที่หมดอายุโดยไม่มีเงินเข้าไม่มีอะไรให้คืน (จ่ายมาช้าเป็นเคส 3)
- ระบบเปลี่ยนสถานะและสร้าง alert ส่วนการโอนเงินคืนจริงเป็นงานแอดมิน

ทดสอบ: `PERMANENT` → `FAILED` → fix-uid → `QUEUED` → `PERMANENT` อีกรอบ → refund → `REFUNDED` → refund ซ้ำ → 409

### เคส 5: ออเดอร์ค้าง / หมดอายุ
reaper รันทุก 5 วินาที ทำเพิ่มจากเคส 1 อีก 2 อย่าง:
- `PENDING_PAYMENT` ที่เลย `expires_at` (15 นาที) → `EXPIRED` พร้อมบันทึกใน timeline
- `QUEUED` นานเกิน SLA (default 300 วินาที) → alert `QUEUE_SLA_BREACH` (ออเดอร์ละ 1 ครั้ง)

**รันซ้ำได้โดยไม่เสียหาย (idempotent):** เงื่อนไขอยู่ใน SQL เลือกเฉพาะที่ยัง `PENDING_PAYMENT` พอเป็น `EXPIRED` แล้วรอบหน้าไม่เจออีก alert กันซ้ำด้วย `NOT EXISTS` และใช้ `FOR UPDATE SKIP LOCKED` กันหลายเครื่องชนกัน

เหตุผลที่เป็น `EXPIRED` ไม่ใช่ลบ: ถ้าลูกค้าจ่ายมาช้า (เคส 3) ต้องรู้ว่าหมดอายุแล้วเพื่อคืนเงิน
ทดสอบ: ตั้ง `expires_at` ย้อนหลัง → เห็น `expired (no payment)` ครั้งเดียว รอหลายรอบก็ไม่ซ้ำ

## Log และ error
- log ทุก request ขึ้นต้นด้วย `order=<เลขออเดอร์>` ค้นตามรอยด้วย `docker compose logs api | grep ORD-...`
- error 500: ลูกค้าเห็นข้อความกลางๆ แต่ log เก็บสาเหตุจริง

## Test

**Unit test** (ไม่ต้องใช้ DB):

```
docker run --rm -v "${PWD}:/src" -w /src golang:1.25-alpine sh -c "go test ./... -v"
```

| ทดสอบด้วย unit test | ทดสอบด้วยการรันจริง (`requests.http` / PowerShell) |
|---|---|
| ตาราง state machine และสถานะสุดท้าย | claim พร้อมกันหลาย bot (เคส 2) |
| รูปแบบ error response | lease หมด → retry → `DELAYED` (เคส 1) |
| regex เลขออเดอร์ | webhook ซ้ำ/ช้า/ลายเซ็นผิด (เคส 3) |
| ลายเซ็น HMAC ของ webhook (6 กรณี) | ผลเติม, fix-uid, refund (เคส 4) |
| สูตร backoff `base × 2^attempts` | reaper ย้าย `EXPIRED` และ alert (เคส 5) |

ข้อสังเกต: สูตร backoff ที่ใช้จริงอยู่ใน SQL เทสต์ `backoffDelay` จึงเป็นการบันทึกสูตรไว้ ไม่ได้พิสูจน์ SQL ส่วนที่ต้องใช้ DB ยังไม่มี automated test ทดสอบโดยรันจริงตามตารางด้านบน

## ข้อจำกัดและสิ่งที่จะปรับปรุง
- ยังไม่มี authentication: `fix-uid`/`refund`/`report-issue` และ endpoint ของ bot เรียกได้โดยไม่ต้องยืนยันตัวตน (ควรให้ `fix-uid` เป็นของเจ้าของออเดอร์, `refund` เป็นของแอดมิน, bot มี token)
- bot ส่ง `SUCCESS` ช้าหลัง lease หมดจะได้ 409 (ของจริงควร reconcile กับเกมก่อนคืนคิว กันเติมซ้ำ)
- `WEBHOOK_SECRET` และรหัส DB ใน compose เป็นค่า dev ต้องเปลี่ยนตอน deploy
- แบ่งหน้าด้วย OFFSET ช้าลงเมื่อข้อมูลมาก (ทางเลือก: keyset pagination)
- `Idempotency-Key` ซ้ำแต่ body ต่าง ตอนนี้คืนของเดิม (ควรตอบ 409)
- `amount` รับเป็น float64 ตอนรับเข้า (ควรใช้ string/decimal)
- reaper อยู่ในโปรเซสเดียวกับ API (ถ้า scale ควรแยกเป็น worker)
- ใช้ `updated_at` เป็นเวลาเข้าคิวแบบประมาณ (ควรมีคอลัมน์ `queued_at`)
- alert กันซ้ำด้วย `NOT EXISTS` ถ้ารันหลายเครื่องพร้อมกันอาจซ้ำได้ในเสี้ยววินาที

## การใช้ AI
ผมใช้ AI (Claude) เป็นผู้ช่วยสอนและเขียนโค้ดตลอดโปรเจกต์ โดยทำทีละขั้นเล็กๆ **AI เป็นคนเสนอโครงสร้าง วิธีแก้ของแต่ละเคส และร่างโค้ดเป็นส่วนใหญ่** ผมเป็นคนเลือก stack (Go + PostgreSQL + Docker Compose), กำหนดวิธีทำงาน (ทีละขั้น พร้อมเหตุผลและตารางอธิบายทุกคำสั่ง), วางโค้ดลงโปรเจกต์ และรันทดสอบทุกขั้นด้วยตัวเอง (PowerShell, `requests.http`, unit test) แล้วนำผลจริงไปให้ AI วินิจฉัยต่อ

สิ่งที่ผลรันจริงเผยว่า AI พลาด:
- เดาชื่อ field ใน JSON ผิด (`game_uid` แทน `gameUid`) จนได้ error `INVALID_BODY`
- เสนอโค้ด webhook ซ้ำกับที่มีอยู่แล้วในโปรเจกต์
- เสนอโครง `requests.http` ที่เดา field ของ `/payment` ผิด

ทุกครั้งผมไม่ได้วางตามทันที แต่รันและดูผลก่อน แล้วค่อยแก้ตามสาเหตุจริงที่เห็นจาก error และโค้ดในโปรเจกต์