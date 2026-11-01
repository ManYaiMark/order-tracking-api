package main

type Status string

const (
	PendingPayment Status = "PENDING_PAYMENT"
	Queued         Status = "QUEUED"
	InProgress     Status = "IN_PROGRESS"
	Completed      Status = "COMPLETED"
	Delayed        Status = "DELAYED"
	Failed         Status = "FAILED"
	Expired        Status = "EXPIRED"
	Refunded       Status = "REFUNDED"
)

// ใครไปไหนได้บ้าง (ไม่อยู่ในตาราง = ห้าม)
var transitions = map[Status][]Status{
	PendingPayment: {Queued, Expired},
	Queued:         {InProgress, Delayed},
	InProgress:     {Completed, Failed, Delayed, Queued}, // Queued = lease หมด/retry
	Delayed:        {Queued, Failed, Refunded},
	Failed:         {Queued, Refunded}, // Queued = แก้ UID แล้วลองใหม่
	Expired:        {Refunded},         // จ่ายมาช้าหลังหมดอายุ
	Completed:      {},
	Refunded:       {},
}

func CanTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}
