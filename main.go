package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Order struct {
	OrderNo   string     `json:"orderNo"`
	GameUID   string     `json:"gameUid"`
	Product   string     `json:"product"`
	Amount    string     `json:"amount"`
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"expiresAt"`
	CreatedAt time.Time  `json:"createdAt"`
}

type createReq struct {
	GameUID string  `json:"gameUid"`
	Product string  `json:"product"`
	Amount  float64 `json:"amount"`
}

const orderCols = `order_no, game_uid, product, amount::text, status, expires_at, created_at`

func scanOrder(row *sql.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.OrderNo, &o.GameUID, &o.Product, &o.Amount, &o.Status, &o.ExpiresAt, &o.CreatedAt)
	return o, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// ORD-<13 หลัก ms>-<3 หลักสุ่ม> ตรงกับตัวอย่างในโจทย์
func newOrderNo() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000))
	return fmt.Sprintf("ORD-%d-%03d", time.Now().UnixMilli(), n.Int64())
}

func createOrder(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeErr(w, 400, "MISSING_IDEMPOTENCY_KEY", "Idempotency-Key header is required")
			return
		}
		var req createReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			req.GameUID == "" || req.Product == "" || req.Amount <= 0 {
			writeErr(w, 400, "INVALID_BODY", "gameUid, product and amount (>0) are required")
			return
		}

		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		defer tx.Rollback()

		o, err := scanOrder(tx.QueryRowContext(ctx, `
			INSERT INTO orders (order_no, idempotency_key, game_uid, product, amount, expires_at)
			VALUES ($1, $2, $3, $4, $5, now() + interval '15 minutes')
			ON CONFLICT (idempotency_key) DO NOTHING
			RETURNING `+orderCols,
			newOrderNo(), key, req.GameUID, req.Product, req.Amount))

		switch {
		case err == nil: // สร้างใหม่
			if _, err = tx.ExecContext(ctx,
				`INSERT INTO order_events (order_no, to_status, reason) VALUES ($1, $2, 'order created')`,
				o.OrderNo, o.Status); err != nil {
				serverErr(w, r, err)
				return
			}
			if err = tx.Commit(); err != nil {
				serverErr(w, r, err)
				return
			}
			log.Printf("order=%s created", o.OrderNo)
			writeJSON(w, 201, o)

		case errors.Is(err, sql.ErrNoRows): // key ซ้ำ → คืนของเดิม
			o, err = scanOrder(tx.QueryRowContext(ctx,
				`SELECT `+orderCols+` FROM orders WHERE idempotency_key = $1`, key))
			if err != nil {
				serverErr(w, r, err)
				return
			}
			log.Printf("order=%s duplicate request (same key)", o.OrderNo)
			writeJSON(w, 200, o)

		default:
			// log.Printf("create order error: %v", err)
			serverErr(w, r, err)
		}
	}
}

var orderNoRe = regexp.MustCompile(`^ORD-[0-9]+-[0-9]+$`)

func getOrder(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := r.PathValue("orderNo")
		if !orderNoRe.MatchString(no) {
			writeErr(w, 400, "INVALID_ORDER_NO", "orderNo must match ORD-<digits>-<digits>")
			return
		}
		o, err := scanOrder(db.QueryRowContext(r.Context(),
			`SELECT `+orderCols+` FROM orders WHERE order_no = $1`, no))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeErr(w, 404, "ORDER_NOT_FOUND", "order not found")
		case err != nil:
			// log.Printf("order=%s get error: %v", no, err)
			serverErr(w, r, err)
		default:
			writeJSON(w, 200, o)
		}
	}
}

// ค่าคงที่ในโค้ดเรา ไม่ได้มาจากผู้ใช้ จึงต่อสตริงใน SQL ได้อย่างปลอดภัย
var tabStatuses = map[string]string{
	"current": `('PENDING_PAYMENT','QUEUED','IN_PROGRESS','DELAYED')`,
	"history": `('COMPLETED','FAILED','EXPIRED','REFUNDED')`,
}

func listOrders(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tab := r.URL.Query().Get("tab")
		if tab == "" {
			tab = "current"
		}
		in, ok := tabStatuses[tab]
		if !ok {
			writeErr(w, 400, "INVALID_TAB", "tab must be current or history")
			return
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 {
				writeErr(w, 400, "INVALID_PAGE", "page must be an integer >= 1")
				return
			}
			page = n
		}
		const pageSize = 10
		ctx := r.Context()

		var total int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM orders WHERE status IN `+in).Scan(&total); err != nil {
			serverErr(w, r, err)
			return
		}
		rows, err := db.QueryContext(ctx,
			`SELECT `+orderCols+` FROM orders WHERE status IN `+in+
				` ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`,
			pageSize, (page-1)*pageSize)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		defer rows.Close()

		items := []Order{} // ใช้ [] เพื่อให้ JSON เป็น [] ไม่ใช่ null
		for rows.Next() {
			var o Order
			if err := rows.Scan(&o.OrderNo, &o.GameUID, &o.Product, &o.Amount,
				&o.Status, &o.ExpiresAt, &o.CreatedAt); err != nil {
				serverErr(w, r, err)
				return
			}
			items = append(items, o)
		}
		if err := rows.Err(); err != nil {
			serverErr(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"items": items, "page": page, "pageSize": pageSize, "total": total,
		})
	}
}

type Event struct {
	From      *string   `json:"from"`
	To        string    `json:"to"`
	Reason    *string   `json:"reason"`
	CreatedAt time.Time `json:"at"`
}

func getTimeline(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := r.PathValue("orderNo")
		if !orderNoRe.MatchString(no) {
			writeErr(w, 400, "INVALID_ORDER_NO", "orderNo must match ORD-<digits>-<digits>")
			return
		}
		ctx := r.Context()
		var exists bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM orders WHERE order_no = $1)`, no).Scan(&exists); err != nil {
			serverErr(w, r, err)
			return
		}
		if !exists {
			writeErr(w, 404, "ORDER_NOT_FOUND", "order not found")
			return
		}
		rows, err := db.QueryContext(ctx,
			`SELECT from_status, to_status, reason, created_at
			FROM order_events WHERE order_no = $1 ORDER BY created_at, id`, no)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		defer rows.Close()

		events := []Event{}
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.From, &e.To, &e.Reason, &e.CreatedAt); err != nil {
				serverErr(w, r, err)
				return
			}
			events = append(events, e)
		}
		if err := rows.Err(); err != nil {
			serverErr(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"orderNo": no, "events": events})
	}
}

// ตัวช่วยจัดการ error และ log

func serverErr(w http.ResponseWriter, r *http.Request, err error) {
	order := r.PathValue("orderNo")
	if order == "" {
		order = "-"
	}
	log.Printf("order=%s ERROR %s %s: %v", order, r.Method, r.URL.Path, err)
	writeErr(w, 500, "INTERNAL", "internal server error")
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

var orderInPath = regexp.MustCompile(`ORD-[0-9]+-[0-9]+`)

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		defer func() {
			if rec := recover(); rec != nil { // กัน panic ทำให้ server ล้ม
				log.Printf("PANIC %s %s: %v", r.Method, r.URL.Path, rec)
				writeErr(sw, 500, "INTERNAL", "internal server error")
			}
			order := orderInPath.FindString(r.URL.Path)
			if order == "" {
				order = "-"
			}
			log.Printf("order=%s %s %s -> %d (%s)", order, r.Method, r.URL.Path,
				sw.status, time.Since(start).Round(time.Millisecond))
		}()
		next.ServeHTTP(sw, r)
	})
}

func claimOrder(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			BotID string `json:"botId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BotID == "" {
			writeErr(w, 400, "INVALID_BODY", "botId is required")
			return
		}
		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		defer tx.Rollback()

		// o, err := scanOrder(tx.QueryRowContext(ctx, `
		// 	WITH next AS (
		// 		SELECT id FROM orders
		// 		WHERE status = 'QUEUED'
		// 		ORDER BY created_at
		// 		FOR UPDATE SKIP LOCKED
		// 		LIMIT 1
		// 	)
		// 	UPDATE orders o
		// 	SET status = 'IN_PROGRESS',
		// 		claimed_by = $1,
		// 		lease_expires_at = now() + interval '2 minutes',
		// 		version = o.version + 1,
		// 		updated_at = now()
		// 	FROM next
		// 	WHERE o.id = next.id
		// 	RETURNING `+orderCols, req.BotID))
		o, err := scanOrder(tx.QueryRowContext(ctx, `
			WITH next AS (
				SELECT id FROM orders
				WHERE status = 'QUEUED'
				AND (next_attempt_at IS NULL OR next_attempt_at <= now())
				ORDER BY created_at
				FOR UPDATE SKIP LOCKED
				LIMIT 1
			)
			UPDATE orders o
			SET status = 'IN_PROGRESS',
				claimed_by = $1,
				lease_expires_at = now() + $2::int * interval '1 second',
				next_attempt_at = NULL,
				version = o.version + 1,
				updated_at = now()
			FROM next
			WHERE o.id = next.id
			RETURNING `+orderCols, req.BotID, leaseSeconds))

		if errors.Is(err, sql.ErrNoRows) { // ไม่มีงานในคิว
			w.WriteHeader(204)
			return
		}
		if err != nil {
			serverErr(w, r, err)
			return
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_events (order_no, from_status, to_status, reason)
			VALUES ($1, 'QUEUED', 'IN_PROGRESS', $2)`,
			o.OrderNo, "claimed by "+req.BotID); err != nil {
			serverErr(w, r, err)
			return
		}
		if err := tx.Commit(); err != nil {
			serverErr(w, r, err)
			return
		}
		log.Printf("order=%s claimed by %s", o.OrderNo, req.BotID)
		writeJSON(w, 200, o)
	}
}

func recordPayment(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := r.PathValue("orderNo")
		if !orderNoRe.MatchString(no) {
			writeErr(w, 400, "INVALID_ORDER_NO", "orderNo must match ORD-<digits>-<digits>")
			return
		}
		var req struct {
			BotID string `json:"botId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BotID == "" {
			writeErr(w, 400, "INVALID_BODY", "botId is required")
			return
		}
		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		defer tx.Rollback()

		// บันทึกได้เฉพาะ: ออเดอร์ IN_PROGRESS และ bot คนนี้เป็นเจ้าของ
		var paymentID int64
		err = tx.QueryRowContext(ctx, `
			INSERT INTO payments (order_no, bot_id, amount)
			SELECT order_no, $2::text, amount FROM orders
			WHERE order_no = $1 AND status = 'IN_PROGRESS' AND claimed_by = $2::text
			ON CONFLICT (order_no) DO NOTHING
			RETURNING id`, no, req.BotID).Scan(&paymentID)

		switch {
		case err == nil:
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO order_events (order_no, from_status, to_status, reason)
				VALUES ($1, 'IN_PROGRESS', 'IN_PROGRESS', $2)`,
				no, "payment recorded by "+req.BotID); err != nil {
				serverErr(w, r, err)
				return
			}
			if err := tx.Commit(); err != nil {
				serverErr(w, r, err)
				return
			}
			log.Printf("order=%s payment recorded by %s", no, req.BotID)
			writeJSON(w, 201, map[string]any{"orderNo": no, "paymentId": paymentID, "botId": req.BotID})

		case errors.Is(err, sql.ErrNoRows): // ไม่ได้ insert → หาสาเหตุ
			var paid, exists bool
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM payments WHERE order_no = $1),
						EXISTS(SELECT 1 FROM orders WHERE order_no = $1)`, no).Scan(&paid, &exists); err != nil {
				serverErr(w, r, err)
				return
			}
			switch {
			case paid:
				log.Printf("order=%s duplicate payment blocked (bot=%s)", no, req.BotID)
				writeErr(w, 409, "DUPLICATE_PAYMENT", "payment already recorded for this order")
			case !exists:
				writeErr(w, 404, "ORDER_NOT_FOUND", "order not found")
			default:
				writeErr(w, 409, "NOT_CLAIMED", "order is not IN_PROGRESS or not claimed by this bot")
			}

		default:
			serverErr(w, r, err)
		}
	}
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

var (
	leaseSeconds = envInt("LEASE_SECONDS", 120)
	maxRetries   = envInt("MAX_RETRIES", 3)
	backoffBase  = envInt("BACKOFF_BASE_SECONDS", 10)
	reaperEvery  = envInt("REAPER_INTERVAL_SECONDS", 10)
)

// bot ส่งสัญญาณว่ายังมีชีวิต → ต่ออายุ lease
func heartbeat(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := r.PathValue("orderNo")
		if !orderNoRe.MatchString(no) {
			writeErr(w, 400, "INVALID_ORDER_NO", "orderNo must match ORD-<digits>-<digits>")
			return
		}
		var req struct {
			BotID string `json:"botId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BotID == "" {
			writeErr(w, 400, "INVALID_BODY", "botId is required")
			return
		}
		res, err := db.ExecContext(r.Context(), `
			UPDATE orders SET lease_expires_at = now() + $3::int * interval '1 second',
							updated_at = now()
			WHERE order_no = $1 AND status = 'IN_PROGRESS' AND claimed_by = $2`,
			no, req.BotID, leaseSeconds)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeErr(w, 409, "LEASE_LOST", "order is not IN_PROGRESS or not claimed by this bot")
			return
		}
		writeJSON(w, 200, map[string]any{"orderNo": no, "leaseSeconds": leaseSeconds})
	}
}

func botResult(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := r.PathValue("orderNo")
		if !orderNoRe.MatchString(no) {
			writeErr(w, 400, "INVALID_ORDER_NO", "orderNo must match ORD-<digits>-<digits>")
			return
		}
		var req struct {
			BotID   string `json:"botId"`
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BotID == "" {
			writeErr(w, 400, "INVALID_BODY", "botId and outcome are required")
			return
		}
		if req.Outcome != "SUCCESS" && req.Outcome != "TRANSIENT" && req.Outcome != "PERMANENT" {
			writeErr(w, 400, "INVALID_OUTCOME", "outcome must be SUCCESS, TRANSIENT or PERMANENT")
			return
		}

		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		defer tx.Rollback()

		// ล็อกแถว + ตรวจว่า bot นี้ถือ lease อยู่จริง
		var attempts int
		err = tx.QueryRowContext(r.Context(), `
			SELECT attempts FROM orders
			WHERE order_no = $1 AND status = 'IN_PROGRESS' AND claimed_by = $2
			FOR UPDATE`, no, req.BotID).Scan(&attempts)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 409, "LEASE_LOST", "order is not IN_PROGRESS or not claimed by this bot")
			return
		} else if err != nil {
			serverErr(w, r, err)
			return
		}

		var to, reason string
		switch req.Outcome {
		case "SUCCESS":
			to, reason = "COMPLETED", "top-up succeeded (bot="+req.BotID+")"
			_, err = tx.ExecContext(r.Context(), `
				UPDATE orders SET status='COMPLETED', claimed_by=NULL, lease_expires_at=NULL,
				version=version+1, updated_at=now() WHERE order_no=$1`, no)
		case "PERMANENT":
			if req.Reason == "" {
				req.Reason = "permanent error"
			}
			to, reason = "FAILED", "permanent failure: "+req.Reason+" (bot="+req.BotID+")"
			_, err = tx.ExecContext(r.Context(), `
				UPDATE orders SET status='FAILED', claimed_by=NULL, lease_expires_at=NULL,
				version=version+1, updated_at=now() WHERE order_no=$1`, no)
		case "TRANSIENT":
			if req.Reason == "" {
				req.Reason = "temporary error"
			}
			if attempts < maxRetries {
				to, reason = "QUEUED", fmt.Sprintf("transient failure: %s; retry %d/%d queued (bot=%s)", req.Reason, attempts+1, maxRetries, req.BotID)
				_, err = tx.ExecContext(r.Context(), `
					UPDATE orders SET status='QUEUED', attempts=attempts+1,
					next_attempt_at = now() + ($2::int * power(2, attempts)::int) * interval '1 second',
					claimed_by=NULL, lease_expires_at=NULL,
					version=version+1, updated_at=now() WHERE order_no=$1`, no, backoffBase)
			} else {
				to, reason = "DELAYED", "transient failure: "+req.Reason+"; max retries reached -> DELAYED (bot="+req.BotID+")"
				_, err = tx.ExecContext(r.Context(), `
					UPDATE orders SET status='DELAYED', next_attempt_at=NULL,
					claimed_by=NULL, lease_expires_at=NULL,
					version=version+1, updated_at=now() WHERE order_no=$1`, no)
				if err == nil {
					_, err = tx.ExecContext(r.Context(),
						`INSERT INTO alerts (order_no, type, message) VALUES ($1,'BOT_UNRESPONSIVE',$2)`, no, reason)
				}
			}
		}
		if err != nil {
			serverErr(w, r, err)
			return
		}
		if !CanTransition(Status("IN_PROGRESS"), Status(to)) {
			serverErr(w, r, fmt.Errorf("illegal transition IN_PROGRESS -> %s", to))
			return
		}
		if _, err = tx.ExecContext(r.Context(), `
			INSERT INTO order_events (order_no, from_status, to_status, reason)
			VALUES ($1,'IN_PROGRESS',$2,$3)`, no, to, reason); err != nil {
			serverErr(w, r, err)
			return
		}
		if err = tx.Commit(); err != nil {
			serverErr(w, r, err)
			return
		}
		log.Printf("order=%s result %s -> %s", no, req.Outcome, to)
		writeJSON(w, 200, map[string]any{"orderNo": no, "status": to})
	}
}

// ลูกค้าแจ้งปัญหา -> บันทึกเป็น alert ให้แอดมินเห็น (ไม่เปลี่ยนสถานะ)
func reportIssue(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := r.PathValue("orderNo")
		if !orderNoRe.MatchString(no) {
			writeErr(w, 400, "INVALID_ORDER_NO", "orderNo must match ORD-<digits>-<digits>")
			return
		}
		var req struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			strings.TrimSpace(req.Message) == "" || len(req.Message) > 500 {
			writeErr(w, 400, "INVALID_BODY", "message is required (max 500 chars)")
			return
		}
		var st string
		err := db.QueryRowContext(r.Context(), `SELECT status FROM orders WHERE order_no=$1`, no).Scan(&st)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "ORDER_NOT_FOUND", "order not found")
			return
		} else if err != nil {
			serverErr(w, r, err)
			return
		}
		if _, err = db.ExecContext(r.Context(),
			`INSERT INTO alerts (order_no, type, message) VALUES ($1,'CUSTOMER_ISSUE',$2)`,
			no, req.Message); err != nil {
			serverErr(w, r, err)
			return
		}
		log.Printf("order=%s customer issue reported (status=%s)", no, st)
		writeJSON(w, 201, map[string]any{"orderNo": no, "status": "RECEIVED"})
	}
}

// แก้ UID แล้วลองใหม่: FAILED -> QUEUED
func fixUID(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := r.PathValue("orderNo")
		if !orderNoRe.MatchString(no) {
			writeErr(w, 400, "INVALID_ORDER_NO", "orderNo must match ORD-<digits>-<digits>")
			return
		}
		var req struct {
			GameUID string `json:"gameUid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.GameUID) == "" {
			writeErr(w, 400, "INVALID_BODY", "gameUid is required")
			return
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		defer tx.Rollback()

		var cur string
		err = tx.QueryRowContext(r.Context(),
			`SELECT status FROM orders WHERE order_no=$1 FOR UPDATE`, no).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "ORDER_NOT_FOUND", "order not found")
			return
		} else if err != nil {
			serverErr(w, r, err)
			return
		}
		if cur != "FAILED" || !CanTransition(Status(cur), Queued) {
			writeErr(w, 409, "INVALID_STATE", "only FAILED orders can be retried with a new UID (current: "+cur+")")
			return
		}
		if _, err = tx.ExecContext(r.Context(), `
			UPDATE orders SET status='QUEUED', game_uid=$2, attempts=0, next_attempt_at=NULL,
			claimed_by=NULL, lease_expires_at=NULL, version=version+1, updated_at=now()
			WHERE order_no=$1`, no, req.GameUID); err != nil {
			serverErr(w, r, err)
			return
		}
		if _, err = tx.ExecContext(r.Context(), `
			INSERT INTO order_events (order_no, from_status, to_status, reason)
			VALUES ($1,'FAILED','QUEUED',$2)`, no, "customer corrected UID -> retry"); err != nil {
			serverErr(w, r, err)
			return
		}
		if err = tx.Commit(); err != nil {
			serverErr(w, r, err)
			return
		}
		log.Printf("order=%s UID corrected -> QUEUED", no)
		writeJSON(w, 200, map[string]any{"orderNo": no, "status": "QUEUED"})
	}
}

// ขอคืนเงิน: FAILED/DELAYED -> REFUNDED + alert ให้แอดมินโอนคืนจริง
func requestRefund(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := r.PathValue("orderNo")
		if !orderNoRe.MatchString(no) {
			writeErr(w, 400, "INVALID_ORDER_NO", "orderNo must match ORD-<digits>-<digits>")
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req) // reason ไม่บังคับ
		if strings.TrimSpace(req.Reason) == "" {
			req.Reason = "customer requested refund"
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		defer tx.Rollback()

		var cur string
		err = tx.QueryRowContext(r.Context(),
			`SELECT status FROM orders WHERE order_no=$1 FOR UPDATE`, no).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "ORDER_NOT_FOUND", "order not found")
			return
		} else if err != nil {
			serverErr(w, r, err)
			return
		}
		// ขอคืนเฉพาะออเดอร์ที่จ่ายแล้วแต่เติมไม่สำเร็จ
		if (cur != "FAILED" && cur != "DELAYED") || !CanTransition(Status(cur), Refunded) {
			writeErr(w, 409, "INVALID_STATE", "refund allowed only for FAILED or DELAYED orders (current: "+cur+")")
			return
		}
		if _, err = tx.ExecContext(r.Context(), `
			UPDATE orders SET status='REFUNDED', claimed_by=NULL, lease_expires_at=NULL,
			next_attempt_at=NULL, version=version+1, updated_at=now() WHERE order_no=$1`, no); err != nil {
			serverErr(w, r, err)
			return
		}
		if _, err = tx.ExecContext(r.Context(), `
			INSERT INTO order_events (order_no, from_status, to_status, reason)
			VALUES ($1,$2,'REFUNDED',$3)`, no, cur, "refund: "+req.Reason); err != nil {
			serverErr(w, r, err)
			return
		}
		if _, err = tx.ExecContext(r.Context(),
			`INSERT INTO alerts (order_no, type, message) VALUES ($1,'REFUND_REQUIRED',$2)`,
			no, "refund requested: "+req.Reason); err != nil {
			serverErr(w, r, err)
			return
		}
		if err = tx.Commit(); err != nil {
			serverErr(w, r, err)
			return
		}
		log.Printf("order=%s refunded from %s", no, cur)
		writeJSON(w, 200, map[string]any{"orderNo": no, "status": "REFUNDED"})
	}
}

type reaped struct {
	orderNo, status, oldBot string
	attempts                int
}

// กวาดออเดอร์ที่ lease หมด: retry (backoff) หรือย้ายเป็น DELAYED + alert
func reapExpiredLeases(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		WITH expired AS (
			SELECT id, claimed_by AS old_bot FROM orders
			WHERE status = 'IN_PROGRESS' AND lease_expires_at < now()
			FOR UPDATE SKIP LOCKED
		)
		UPDATE orders o SET
			status = CASE WHEN o.attempts < $1 THEN 'QUEUED' ELSE 'DELAYED' END,
			attempts = CASE WHEN o.attempts < $1 THEN o.attempts + 1 ELSE o.attempts END,
			next_attempt_at = CASE WHEN o.attempts < $1
				THEN now() + ($2::int * power(2, o.attempts)::int) * interval '1 second' END,
			claimed_by = NULL, lease_expires_at = NULL,
			version = o.version + 1, updated_at = now()
		FROM expired e
		WHERE o.id = e.id
		RETURNING o.order_no, o.status, o.attempts, e.old_bot`, maxRetries, backoffBase)
	if err != nil {
		return err
	}
	var list []reaped
	for rows.Next() {
		var x reaped
		var oldBot sql.NullString
		if err := rows.Scan(&x.orderNo, &x.status, &x.attempts, &oldBot); err != nil {
			rows.Close()
			return err
		}
		x.oldBot = oldBot.String
		list = append(list, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, x := range list {
		var reason string
		if x.status == "QUEUED" {
			reason = fmt.Sprintf("lease expired (bot=%s); retry %d/%d queued", x.oldBot, x.attempts, maxRetries)
			log.Printf("order=%s %s", x.orderNo, reason)
		} else {
			reason = fmt.Sprintf("lease expired (bot=%s); max retries reached -> DELAYED", x.oldBot)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO alerts (order_no, type, message) VALUES ($1, 'BOT_UNRESPONSIVE', $2)`,
				x.orderNo, reason); err != nil {
				return err
			}
			log.Printf("order=%s ALERT %s", x.orderNo, reason)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_events (order_no, from_status, to_status, reason)
			VALUES ($1, 'IN_PROGRESS', $2, $3)`, x.orderNo, x.status, reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func startReaper(db *sql.DB) {
	go func() {
		t := time.NewTicker(time.Duration(reaperEvery) * time.Second)
		defer t.Stop()
		for range t.C {
			ctx := context.Background()
			if err := reapExpiredLeases(ctx, db); err != nil {
				log.Printf("ERROR reaper leases: %v", err)
			}
			if err := expirePendingOrders(ctx, db); err != nil {
				log.Printf("ERROR reaper expire: %v", err)
			}
			if err := alertStaleQueue(ctx, db); err != nil {
				log.Printf("ERROR reaper sla: %v", err)
			}
		}
	}()
}

var queueSLA = envInt("QUEUE_SLA_SECONDS", 300)

// PENDING_PAYMENT ที่เลยเวลา → EXPIRED (รันซ้ำได้ ไม่เสียหาย เพราะเลือกเฉพาะที่ยัง PENDING_PAYMENT)
func expirePendingOrders(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		WITH due AS (
			SELECT id FROM orders
			WHERE status = 'PENDING_PAYMENT' AND expires_at < now()
			FOR UPDATE SKIP LOCKED
		)
		UPDATE orders o SET status = 'EXPIRED', version = o.version + 1, updated_at = now()
		FROM due WHERE o.id = due.id
		RETURNING o.order_no`)
	if err != nil {
		return err
	}
	var nos []string
	for rows.Next() {
		var no string
		if err := rows.Scan(&no); err != nil {
			rows.Close()
			return err
		}
		nos = append(nos, no)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, no := range nos {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_events (order_no, from_status, to_status, reason)
			VALUES ($1, 'PENDING_PAYMENT', 'EXPIRED', 'payment window elapsed')`, no); err != nil {
			return err
		}
		log.Printf("order=%s expired (no payment)", no)
	}
	return tx.Commit()
}

// QUEUED นานเกิน SLA → สร้าง alert (ออเดอร์ละ 1 ครั้ง)
func alertStaleQueue(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		INSERT INTO alerts (order_no, type, message)
		SELECT o.order_no, 'QUEUE_SLA_BREACH', 'queued longer than ' || $1::int || 's'
		FROM orders o
		WHERE o.status = 'QUEUED'
		  	AND o.updated_at < now() - $1::int * interval '1 second'
			AND NOT EXISTS (SELECT 1 FROM alerts a
				WHERE a.order_no = o.order_no AND a.type = 'QUEUE_SLA_BREACH')
		RETURNING order_no`, queueSLA)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var no string
		if err := rows.Scan(&no); err != nil {
			return err
		}
		log.Printf("order=%s ALERT queued longer than %ds", no, queueSLA)
	}
	return rows.Err()
}

var webhookSecret = os.Getenv("WEBHOOK_SECRET")

func validSignature(secret string, body []byte, sigHex string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got, err := hex.DecodeString(sigHex)
	return err == nil && hmac.Equal(got, mac.Sum(nil))
}

func bankWebhook(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeErr(w, 400, "INVALID_BODY", "cannot read body")
			return
		}
		// 1) ตรวจลายเซ็นก่อนทำอย่างอื่น
		if !validSignature(webhookSecret, body, r.Header.Get("X-Signature")) {
			writeErr(w, 401, "INVALID_SIGNATURE", "signature mismatch")
			return
		}
		var ev struct {
			EventID string `json:"eventId"`
			OrderNo string `json:"orderNo"`
			Type    string `json:"type"`
		}
		if err := json.Unmarshal(body, &ev); err != nil || ev.EventID == "" || !orderNoRe.MatchString(ev.OrderNo) {
			writeErr(w, 400, "INVALID_BODY", "eventId and a valid orderNo are required")
			return
		}
		if ev.Type != "payment.succeeded" {
			writeErr(w, 400, "UNSUPPORTED_EVENT", "only payment.succeeded is supported")
			return
		}

		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		defer tx.Rollback()

		// 2) event ซ้ำ? (ตอบ 200 เพื่อให้ธนาคารเลิกส่งซ้ำ)
		res, err := tx.ExecContext(ctx,
			`INSERT INTO webhook_events (event_id, order_no, type) VALUES ($1, $2, $3)
			ON CONFLICT (event_id) DO NOTHING`, ev.EventID, ev.OrderNo, ev.Type)
		if err != nil {
			serverErr(w, r, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			log.Printf("order=%s webhook %s duplicate ignored", ev.OrderNo, ev.EventID)
			writeJSON(w, 200, map[string]string{"status": "duplicate"})
			return
		}

		// 3) ล็อกออเดอร์แล้วดูสถานะ "ตอนนี้" ก่อนตัดสินใจ
		var from Status
		err = tx.QueryRowContext(ctx,
			`SELECT status FROM orders WHERE order_no = $1 FOR UPDATE`, ev.OrderNo).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "ORDER_NOT_FOUND", "order not found") // rollback ลบ event ด้วย
			return
		}
		if err != nil {
			serverErr(w, r, err)
			return
		}

		var next Status
		var reason, alertType string
		switch from {
		case PendingPayment:
			next, reason = Queued, "bank payment confirmed ("+ev.EventID+")"
		case Expired: // มาช้าหลังหมดอายุ → คืนเงิน
			next, reason, alertType = Refunded, "late payment after expiry -> refund ("+ev.EventID+")", "REFUND_REQUIRED"
		default: // สถานะอื่น = ซ้ำซ้อน/ผิดลำดับ ไม่แตะสถานะ แต่แจ้งแอดมิน
			reason, alertType = "payment event in state "+string(from)+" ignored ("+ev.EventID+")", "UNEXPECTED_PAYMENT"
		}

		outcome := "ignored"
		if next != "" && CanTransition(from, next) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE orders SET status = $2, version = version + 1, updated_at = now()
				WHERE order_no = $1`, ev.OrderNo, string(next)); err != nil {
				serverErr(w, r, err)
				return
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO order_events (order_no, from_status, to_status, reason)
				VALUES ($1, $2, $3, $4)`, ev.OrderNo, string(from), string(next), reason); err != nil {
				serverErr(w, r, err)
				return
			}
			outcome = string(next)
		}
		if alertType != "" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO alerts (order_no, type, message) VALUES ($1, $2, $3)`,
				ev.OrderNo, alertType, reason); err != nil {
				serverErr(w, r, err)
				return
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE webhook_events SET outcome = $2 WHERE event_id = $1`, ev.EventID, outcome); err != nil {
			serverErr(w, r, err)
			return
		}
		if err := tx.Commit(); err != nil {
			serverErr(w, r, err)
			return
		}
		log.Printf("order=%s webhook %s %s -> %s", ev.OrderNo, ev.EventID, from, outcome)
		writeJSON(w, 200, map[string]string{"status": "processed", "outcome": outcome})
	}
}

func main() {
	webhookSecret = os.Getenv("WEBHOOK_SECRET")
	if webhookSecret == "" {
		log.Fatal("WEBHOOK_SECRET is required")
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	startReaper(db)

	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			writeErr(w, 503, "DB_DOWN", "database unreachable")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /webhooks/bank", bankWebhook(db))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 404, "NOT_FOUND", "route not found")
	})

	mux.HandleFunc("POST /orders", createOrder(db))
	mux.HandleFunc("GET /orders/{orderNo}", getOrder(db))
	mux.HandleFunc("GET /orders", listOrders(db))
	mux.HandleFunc("GET /orders/{orderNo}/timeline", getTimeline(db))
	mux.HandleFunc("POST /orders/{orderNo}/report-issue", reportIssue(db))
	mux.HandleFunc("POST /orders/{orderNo}/fix-uid", fixUID(db))
	mux.HandleFunc("POST /orders/{orderNo}/refund", requestRefund(db))

	mux.HandleFunc("POST /bot/claim", claimOrder(db))
	mux.HandleFunc("POST /bot/orders/{orderNo}/payment", recordPayment(db))
	mux.HandleFunc("POST /bot/orders/{orderNo}/heartbeat", heartbeat(db))
	mux.HandleFunc("POST /bot/orders/{orderNo}/result", botResult(db))

	log.Println("listening :8080")
	log.Fatal(http.ListenAndServe(":8080", withLogging(mux)))
}

func backoffDelay(base, attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	return time.Duration(base) * time.Second * time.Duration(1<<attempts)
}
