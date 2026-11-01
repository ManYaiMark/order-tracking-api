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
			writeErr(w, 500, "INTERNAL", "database error")
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
				writeErr(w, 500, "INTERNAL", "database error")
				return
			}
			if err = tx.Commit(); err != nil {
				writeErr(w, 500, "INTERNAL", "database error")
				return
			}
			log.Printf("order=%s created", o.OrderNo)
			writeJSON(w, 201, o)

		case errors.Is(err, sql.ErrNoRows): // key ซ้ำ → คืนของเดิม
			o, err = scanOrder(tx.QueryRowContext(ctx,
				`SELECT `+orderCols+` FROM orders WHERE idempotency_key = $1`, key))
			if err != nil {
				writeErr(w, 500, "INTERNAL", "database error")
				return
			}
			log.Printf("order=%s duplicate request (same key)", o.OrderNo)
			writeJSON(w, 200, o)

		default:
			log.Printf("create order error: %v", err)
			writeErr(w, 500, "INTERNAL", "database error")
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
			log.Printf("order=%s get error: %v", no, err)
			writeErr(w, 500, "INTERNAL", "database error")
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
			writeErr(w, 500, "INTERNAL", "database error")
			return
		}
		rows, err := db.QueryContext(ctx,
			`SELECT `+orderCols+` FROM orders WHERE status IN `+in+
				` ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`,
			pageSize, (page-1)*pageSize)
		if err != nil {
			writeErr(w, 500, "INTERNAL", "database error")
			return
		}
		defer rows.Close()

		items := []Order{} // ใช้ [] เพื่อให้ JSON เป็น [] ไม่ใช่ null
		for rows.Next() {
			var o Order
			if err := rows.Scan(&o.OrderNo, &o.GameUID, &o.Product, &o.Amount,
				&o.Status, &o.ExpiresAt, &o.CreatedAt); err != nil {
				writeErr(w, 500, "INTERNAL", "database error")
				return
			}
			items = append(items, o)
		}
		if err := rows.Err(); err != nil {
			writeErr(w, 500, "INTERNAL", "database error")
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
			writeErr(w, 500, "INTERNAL", "database error")
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
			writeErr(w, 500, "INTERNAL", "database error")
			return
		}
		defer rows.Close()

		events := []Event{}
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.From, &e.To, &e.Reason, &e.CreatedAt); err != nil {
				writeErr(w, 500, "INTERNAL", "database error")
				return
			}
			events = append(events, e)
		}
		if err := rows.Err(); err != nil {
			writeErr(w, 500, "INTERNAL", "database error")
			return
		}
		writeJSON(w, 200, map[string]any{"orderNo": no, "events": events})
	}
}

func main() {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
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
	mux.HandleFunc("POST /orders", createOrder(db))
	mux.HandleFunc("GET /orders/{orderNo}", getOrder(db))
	mux.HandleFunc("GET /orders", listOrders(db))
	mux.HandleFunc("GET /orders/{orderNo}/timeline", getTimeline(db))

	log.Println("listening :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
