# Order Tracking API

## วิธีรัน
docker compose up --build

## การออกแบบ (Design decisions)
### State machine
### เคส 1: Bot ล่ม
### เคส 2: Bot แย่งออเดอร์
### เคส 3: Webhook ซ้ำ
### เคส 4: เติมไม่สำเร็จ
### เคส 5: ออเดอร์ค้าง/หมดอายุ

## ข้อจำกัดและสิ่งที่จะปรับปรุง
## การใช้ AI

### เคส 2: Bot แย่งออเดอร์
เลือก `SELECT ... FOR UPDATE SKIP LOCKED` ใน SQL เดียวกับ UPDATE
เหตุผล: เลือก+ล็อก+เปลี่ยนสถานะเป็น atomic ไม่มีช่องให้แย่ง
ทดสอบ: 10 บอทพร้อมกัน ต่อ 1 ออเดอร์ → ชนะ 1 ตัวเสมอ
ทางเลือกที่ไม่เลือก: optimistic lock (version) ต้อง retry เมื่อชน