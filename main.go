package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
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
			if err := reapExpiredLeases(context.Background(), db); err != nil {
				log.Printf("ERROR reaper: %v", err)
			}
		}
	}()
}

func main() {
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

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 404, "NOT_FOUND", "route not found")
	})

	mux.HandleFunc("POST /orders", createOrder(db))
	mux.HandleFunc("GET /orders/{orderNo}", getOrder(db))
	mux.HandleFunc("GET /orders", listOrders(db))
	mux.HandleFunc("GET /orders/{orderNo}/timeline", getTimeline(db))

	mux.HandleFunc("POST /bot/claim", claimOrder(db))
	mux.HandleFunc("POST /bot/orders/{orderNo}/payment", recordPayment(db))
	mux.HandleFunc("POST /bot/orders/{orderNo}/heartbeat", heartbeat(db))

	log.Println("listening :8080")
	log.Fatal(http.ListenAndServe(":8080", withLogging(mux)))
}
