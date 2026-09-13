package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPaymentSettlementWaitsForConcurrentWriter(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "runtime"
		if existing {
			name = "existing_database"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shop.db")
			db, err := New(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			store, orderID, productID := seedLedgerOrder(t, db, 250)
			settlementDB := db
			if existing {
				settlementDB, err = OpenReadWriteExisting(path)
				if err != nil {
					t.Fatal(err)
				}
				defer settlementDB.Close()
				store = NewSQLOrderStore(settlementDB)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			writer, err := db.Conn().BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback()
			if _, err := writer.ExecContext(ctx,
				`UPDATE products SET stock = stock WHERE id = ?`, productID); err != nil {
				t.Fatal(err)
			}

			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				done <- store.UpdateOrderStatus(ctx, orderID,
					OrderStatusPending, OrderStatusPaid, PaymentMethodStars, "contended-charge")
			}()
			<-started

			// A read-then-write DEFERRED transaction exhausts the settlement
			// retries in about 0.9s. Brief contention below busy_timeout must
			// instead wait for the writer before reading the order snapshot.
			hold := time.NewTimer(1500 * time.Millisecond)
			defer hold.Stop()
			select {
			case err := <-done:
				t.Fatalf("settlement returned before the concurrent writer released its lock: %v", err)
			case <-hold.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := writer.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("settlement after the concurrent writer committed: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}

			order, err := store.GetOrder(ctx, orderID)
			if err != nil {
				t.Fatal(err)
			}
			if order.Status != OrderStatusPaid || order.PaymentState != PaymentStateSettled {
				t.Fatalf("order was not settled: %+v", order)
			}
			attempts, err := NewSQLPaymentLedgerStore(settlementDB).ListPaymentAttempts(ctx, orderID)
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != 1 || attempts[0].Status != "succeeded" || attempts[0].AmountMinor != 250 {
				t.Fatalf("payment attempts = %+v; want one successful capture of 250 Stars", attempts)
			}
			var stock int
			if err := db.Conn().QueryRowContext(ctx,
				`SELECT stock FROM products WHERE id = ?`, productID).Scan(&stock); err != nil {
				t.Fatal(err)
			}
			if stock != 98 {
				t.Fatalf("stock = %d; want 98 after one settlement", stock)
			}
		})
	}
}
