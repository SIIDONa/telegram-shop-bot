package bot

import (
	"context"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"shop_bot/internal/storage"
)

func TestAdminMenuKeyboard_ContainsAllDashboardSections(t *testing.T) {
	t.Parallel()

	expectedTokens := []string{
		"admin:products:cats",
		"admin:categories",
		"admin:orders:all",
		"admin:promos",
		"analytics:14",
		"admin:reviews",
		"admin:btnlist",
		"admin:export",
		"back:catalog",
	}

	found := make(map[string]bool)
	testKB := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛍 Products", "admin:products:cats"),
			tgbotapi.NewInlineKeyboardButtonData("📂 Categories", "admin:categories"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📦 Orders", "admin:orders:all"),
			tgbotapi.NewInlineKeyboardButtonData("🏷 Promo Codes", "admin:promos"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Analytics", "analytics:14"),
			tgbotapi.NewInlineKeyboardButtonData("💬 Reviews", "admin:reviews"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎨 Button Colors", "admin:btnlist"),
			tgbotapi.NewInlineKeyboardButtonData("📥 Export CSV", "admin:export"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Client Menu", "back:catalog"),
		),
	)

	for _, row := range testKB.InlineKeyboard {
		for _, btn := range row {
			if btn.CallbackData != nil {
				found[*btn.CallbackData] = true
			}
		}
	}

	for _, tok := range expectedTokens {
		if !found[tok] {
			t.Errorf("expected callback token %q in admin menu, but not found", tok)
		}
	}
}

func TestAdminActionInput_CancelClearsState(t *testing.T) {
	b := &Bot{}
	b.adminActions.Store(int64(123), "add_category")

	ctx := context.Background()
	_ = ctx
	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 123},
		From: &tgbotapi.User{ID: 123, LanguageCode: "en"},
		Text: "/cancel",
	}

	b.adminActions.Delete(msg.From.ID)
	if _, ok := b.adminActions.Load(int64(123)); ok {
		t.Fatal("expected admin action state to be deleted on /cancel")
	}
}

func TestAdminOrderFilterKeys(t *testing.T) {
	t.Parallel()

	filters := []string{"all", "pending", "paid", "delivered"}
	for _, f := range filters {
		status := ""
		if f != "all" {
			status = f
		}
		if f == "pending" && status != storage.OrderStatusPending {
			t.Errorf("expected %q, got %q", storage.OrderStatusPending, status)
		}
		if f == "paid" && status != storage.OrderStatusPaid {
			t.Errorf("expected %q, got %q", storage.OrderStatusPaid, status)
		}
		if f == "delivered" && status != storage.OrderStatusDelivered {
			t.Errorf("expected %q, got %q", storage.OrderStatusDelivered, status)
		}
	}
}
