package bot

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestOrderConfirmDisplaysStoredTotals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		priceUSD  float64
		promo     string
		wantUSD   string
		wantStars int
	}{
		{name: "regular", priceUSD: 10, wantUSD: "10.00", wantStars: 500},
		{name: "promo", priceUSD: 10, promo: "SAVE10", wantUSD: "9.00", wantStars: 450},
		{name: "promo rounds Stars down", priceUSD: 1.03, promo: "SAVE10", wantUSD: "0.93", wantStars: 45},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newE2EEnv(t)
			const buyer = int64(1001)
			if _, err := e.db.Conn().Exec(`UPDATE products SET price_usd = ? WHERE id = ?`, tc.priceUSD, e.prodReg); err != nil {
				t.Fatal(err)
			}
			e.cmd(buyer, "/start", "en")
			e.cb(buyer, fmt.Sprintf("cart:add:%d", e.prodReg), "en")
			confirm := "order:confirm"
			if tc.promo != "" {
				confirm += ":promo:" + tc.promo
			}
			calls := e.cb(buyer, confirm, "en")
			orderID := e.qInt(`SELECT MAX(id) FROM orders WHERE user_id = ?`, buyer)
			if got := e.qInt(`SELECT total_stars FROM orders WHERE id = ?`, orderID); got != int64(tc.wantStars) {
				t.Fatalf("stored Stars = %d, want %d", got, tc.wantStars)
			}
			if got := e.qStr(`SELECT printf('%.2f', total_usd) FROM orders WHERE id = ?`, orderID); got != tc.wantUSD {
				t.Fatalf("stored USD = %s, want %s", got, tc.wantUSD)
			}

			payment := requireRender(t, calls, fmt.Sprintf("pay:stars:%d", orderID))
			wantTotal := fmt.Sprintf("$%s / %d ⭐", tc.wantUSD, tc.wantStars)
			if !strings.Contains(payment.Params.Get("text"), "To pay: "+wantTotal) {
				t.Errorf("payment summary = %q, want total %s", payment.Params.Get("text"), wantTotal)
			}
			var markup tgbotapi.InlineKeyboardMarkup
			if err := json.Unmarshal([]byte(payment.markup()), &markup); err != nil {
				t.Fatal(err)
			}
			for _, btn := range []struct {
				callback string
				amount   string
			}{
				{callback: fmt.Sprintf("pay:stars:%d", orderID), amount: fmt.Sprintf("(%d ⭐)", tc.wantStars)},
				{callback: fmt.Sprintf("pay:crypto:%d", orderID), amount: "($" + tc.wantUSD + ")"},
			} {
				found := false
				for _, row := range markup.InlineKeyboard {
					for _, button := range row {
						if button.CallbackData != nil && *button.CallbackData == btn.callback {
							found = true
							if !strings.Contains(button.Text, btn.amount) {
								t.Errorf("button %s = %q, want amount %s", btn.callback, button.Text, btn.amount)
							}
						}
					}
				}
				if !found {
					t.Errorf("payment button %s missing", btn.callback)
				}
			}
			admin := requireCall(t, calls, "sendMessage", "New order #")
			if admin.Params.Get("chat_id") != strconv.FormatInt(e2eAdminID, 10) || !strings.Contains(admin.Params.Get("text"), wantTotal) {
				t.Errorf("admin notification = %v, want admin total %s", admin.Params, wantTotal)
			}
			invoice := requireCall(t, e.cb(buyer, fmt.Sprintf("pay:stars:%d", orderID), "en"), "sendInvoice", "")
			var prices []tgbotapi.LabeledPrice
			if err := json.Unmarshal([]byte(invoice.Params.Get("prices")), &prices); err != nil {
				t.Fatal(err)
			}
			if len(prices) != 1 || prices[0].Amount != tc.wantStars {
				t.Errorf("invoice prices = %+v, want %d Stars", prices, tc.wantStars)
			}
		})
	}
}
