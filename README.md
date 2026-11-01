# Order Tracking API

API ติดตามออเดอร์ร้านเติมเกม (bot จ่ายเงิน + เติมเกมอัตโนมัติ)
Stack: Go (net/http) + PostgreSQL + Docker Compose

## วิธีรัน
```
docker compose up --build
curl localhost:8080/health
```
ทดสอบ API ด้วยไฟล์ `requests.http` (VS Code + ส่วนเสริม REST Client)

## Endpoints
| Method | Path | หน้าที่ |
|---|---|---|
| POST | /orders | สร้างออเดอร์ (ต้องมี header `Idempotency-Key`) |
| GET | /orders/:orderNo | ดูออเดอร์ (400 ถ้า format ผิด, 404 ถ้าไม่พบ) |
| GET | /orders?tab=current\|history&page= | รายการแบ่งหน้า ใหม่ → เก่า |
| GET | /orders/:orderNo/timeline | ประวัติการเปลี่ยนสถานะ |
| POST | /bot/claim | bot ขอรับงานถัดไป |
| POST | /bot/orders/:orderNo/heartbeat | bot ต่ออายุ lease |
| POST | /bot/orders/:orderNo/payment | bot บันทึกการจ่ายเงิน |

## Design decisions

### Schema
- `orders` เก็บสถานะปัจจุบัน, `order_events` เก็บประวัติทุกการเปลี่ยนสถานะ (ใช้ทำ timeline)
- ให้ DB กันข้อมูลผิด: `UNIQUE` (order_no, idempotency_key), `CHECK` (รูปแบบ order_no, status, amount > 0)
- เงินใช้ `NUMERIC` ไม่ใช้ float

### Idempotency (POST /orders)
`INSERT ... ON CONFLICT (idempotency_key) DO NOTHING` แล้วคืนออเดอร์เดิมถ้าซ้ำ (201 ครั้งแรก, 200 ครั้งถัดไป)
เหตุผล: ให้ DB ตัดสินเอง ปลอดภัยกว่า "เช็กก่อนแล้ว insert" ที่เกิด race เมื่อยิงพร้อมกัน

### State machine
```
PENDING_PAYMENT → QUEUED → IN_PROGRESS → COMPLETED
PENDING_PAYMENT → EXPIRED → REFUNDED
IN_PROGRESS → QUEUED (lease หมด retry) / DELAYED / FAILED
FAILED → QUEUED (แก้ UID แล้วลองใหม่) / REFUNDED
DELAYED → QUEUED / FAILED / REFUNDED
```
กฎอยู่ที่เดียว (`state.go`) มี unit test
- `IN_PROGRESS → QUEUED`: bot เงียบเกิน lease จึงคืนเข้าคิวเพื่อ retry
- `FAILED → QUEUED`: เฉพาะ error ที่แก้ได้ เช่น ลูกค้าแก้ UID
- `EXPIRED → REFUNDED`: ลูกค้าจ่ายมาช้าหลังหมดอายุ ต้องคืนเงิน
- `COMPLETED` และ `REFUNDED` เป็นสถานะสุดท้าย

### เคส 1: Bot ล่ม / ค้าง
ใช้ **lease + heartbeat**: ตอน claim ได้ lease (`lease_expires_at`) bot ต้องส่ง heartbeat เพื่อต่ออายุ
reaper (goroutine รันเป็นรอบ) กวาดออเดอร์ที่ lease หมด: retry แบบ exponential backoff (5s, 10s, 20s) ไม่เกิน 3 ครั้ง
ถ้ายังไม่ผ่านย้ายเป็น `DELAYED` และบันทึก alert (ตาราง `alerts`)
เหตุผล: ไม่ต้องเดาว่า bot ตายจริงหรือไม่ แค่ดูว่าเงียบเกินเวลา และ bot เก่าที่ฟื้นมาทีหลังจะต่อ lease/จ่ายเงินไม่ได้
ค่าเวลาตั้งผ่าน env (compose ใช้ค่าสั้นเพื่อ demo, ค่า default ในโค้ดใกล้ของจริง)

ทดสอบ: จำลอง bot รับงานแล้วเงียบ 4 รอบ
log: retry 1/3 (รอ ~5s) → retry 2/3 (รอ ~10s) → retry 3/3 (รอ ~20s) → ALERT ... DELAYED
(ตาราง `alerts` มีแถว BOT_UNRESPONSIVE, timeline เห็นทุกรอบ)

### เคส 2: Bot สองตัวแย่งออเดอร์
- ด่านที่ 1: claim แบบ atomic ใน SQL เดียว `SELECT ... FOR UPDATE SKIP LOCKED` + `UPDATE` bot ที่มาทีหลังจะข้ามแถวที่ถูกล็อกไปเอง
- ด่านที่ 2: ตาราง `payments` มี `UNIQUE(order_no)` ต่อให้โค้ดพลาด DB ก็ไม่ยอมให้จ่ายซ้ำ
- ทดสอบ: ยิง 10 bot พร้อมกันต่อ 1 ออเดอร์ → ชนะ 1 ตัวเสมอ ที่เหลือได้ 204
- ทางเลือกที่ไม่เลือก: optimistic lock (version) ต้องคอย retry เมื่อชน

### เคส 3: Webhook ธนาคารซ้ำ/ช้า


### เคส 4: เติมไม่สำเร็จ


### เคส 5: ออเดอร์ค้าง / หมดอายุ
งานเบื้องหลัง (reaper) รันทุก 5 วินาที ทำ 2 อย่างเพิ่มจากเคส 1:
- `PENDING_PAYMENT` ที่เลย `expires_at` (15 นาที) → `EXPIRED` พร้อมบันทึกใน timeline
- `QUEUED` นานเกิน SLA (default 300s) → สร้าง alert `QUEUE_SLA_BREACH` (ออเดอร์ละ 1 ครั้ง)

**รันซ้ำได้โดยไม่เสียหาย (idempotent):** เงื่อนไขอยู่ใน SQL เลือกเฉพาะที่ยัง `PENDING_PAYMENT`
พอเป็น `EXPIRED` แล้วรอบหน้าไม่เจออีก ส่วน alert กันซ้ำด้วย `NOT EXISTS`
เลือก `FOR UPDATE SKIP LOCKED` เพื่อรันหลายเครื่องแล้วไม่ชนกัน

เหตุผลที่เป็น `EXPIRED` ไม่ใช่ลบ: ถ้าลูกค้าจ่ายมาช้า (เคส 3) ต้องรู้ว่าหมดอายุแล้วเพื่อคืนเงิน
ทดสอบ: ตั้ง `expires_at` ย้อนหลัง → เห็น `expired (no payment)` ครั้งเดียว รอหลายรอบก็ไม่ซ้ำ

## Error format และ log
ทุก error: `{"error":{"code":"...","message":"..."}}`
log ทุก request ขึ้นต้นด้วย `order=<เลขออเดอร์>` ค้นตามรอยด้วย `docker compose logs api | grep ORD-...`
error 500 ลูกค้าเห็นข้อความกลางๆ แต่ log เก็บสาเหตุจริง

## ข้อจำกัดและสิ่งที่จะปรับปรุง
- รายการแบ่งหน้าด้วย OFFSET ช้าลงเมื่อข้อมูลมาก (ทางเลือก: keyset pagination)
- Idempotency-Key ซ้ำแต่ body ต่าง ตอนนี้คืนของเดิม (ควรตอบ 409)
- `amount` รับเป็น float64 ตอนรับเข้า (ควรใช้ string/decimal)
- reaper อยู่ในโปรเซสเดียวกับ API (ถ้า scale ควรแยกเป็น worker)
- ยังไม่มี authentication ของ bot และ admin
- ใช้ `updated_at` เป็นเวลาเข้าคิวแบบประมาณ (ควรมีคอลัมน์ `queued_at` โดยเฉพาะ)
- alert กันซ้ำด้วย NOT EXISTS: ถ้ารันหลายเครื่องพร้อมกันอาจซ้ำได้ในเสี้ยววินาที

## การใช้ AI
ใช้ AI ช่วยร่างโครงและโค้ดทีละขั้น แล้ว **ตรวจและทดสอบเอง** เช่น
- รันทดสอบ 10 bot พร้อมกันเพื่อยืนยันว่าไม่มีการแย่งงาน
- ทดสอบ insert ซ้ำตรงเข้า DB เพื่อยืนยัน unique constraint
- จำลอง bot ล่มและดู log retry/backoff จนถึง DELAYED
- ตัดสินใจเองเรื่องตาราง state machine และเหตุผลของแต่ละเส้นทาง