package bot

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"shop_bot/internal/service"
	"shop_bot/internal/storage"
)

func (b *Bot) isAdmin(userID int64) bool {
	for _, id := range b.cfg.AdminIDs {
		if id == userID {
			return true
		}
	}
	return false
}

func (b *Bot) handleAdmin(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	b.sendAdminMenu(msg.Chat.ID, 0, msg.From.LanguageCode)
}

func (b *Bot) handleAddProduct(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	ctx := context.Background()
	_ = b.fsm.SetAddProductState(ctx, msg.From.ID, &storage.AddProductState{Step: storage.StepName, CreatedAt: time.Now()}, 30*time.Minute)
	b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(msg.From.LanguageCode, "admin_add_product_name")))
}

func (b *Bot) handleAddProductStep(msg *tgbotapi.Message) bool {
	ctx := context.Background()
	if msg.Text == "/cancel" {
		state, _ := b.fsm.GetAddProductState(ctx, msg.From.ID)
		_ = b.fsm.DelAddProductState(ctx, msg.From.ID)
		if state != nil {
			b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(msg.From.LanguageCode, "admin_cancelled")))
			return true
		}
		return false
	}

	state, _ := b.fsm.GetAddProductState(ctx, msg.From.ID)
	if state == nil {
		return false
	}

	chatID := msg.Chat.ID
	lang := msg.From.LanguageCode
	switch state.Step {
	case storage.StepName:
		state.Name = msg.Text
		state.Step = storage.StepDescription
		_ = b.fsm.SetAddProductState(ctx, msg.From.ID, state, 30*time.Minute)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_add_product_description")))
	case storage.StepDescription:
		state.Description = msg.Text
		state.Step = storage.StepPriceUSD
		_ = b.fsm.SetAddProductState(ctx, msg.From.ID, state, 30*time.Minute)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_add_product_price")))
	case storage.StepPriceUSD:
		p, _ := strconv.ParseFloat(msg.Text, 64)
		state.PriceUSD = p
		state.Step = storage.StepStock
		_ = b.fsm.SetAddProductState(ctx, msg.From.ID, state, 30*time.Minute)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_add_product_stock")))
	case storage.StepStock:
		s, _ := strconv.Atoi(msg.Text)
		state.Stock = s
		state.Step = storage.StepPhoto
		_ = b.fsm.SetAddProductState(ctx, msg.From.ID, state, 30*time.Minute)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_photo_prompt")))
	case storage.StepPhoto:
		b.handleWizardPhotoStep(ctx, msg, state)
	case storage.StepCategory:
		id, _ := strconv.ParseInt(msg.Text, 10, 64)
		state.CategoryID = id
		state.Step = storage.StepSubType
		_ = b.fsm.SetAddProductState(ctx, msg.From.ID, state, 30*time.Minute)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_sub_type_prompt")))
	case storage.StepSubType:
		// "2" = 30-day Stars subscription, anything else = regular product.
		if strings.TrimSpace(msg.Text) == "2" {
			state.SubPeriodDays = 30
		}
		_ = b.fsm.SetAddProductState(ctx, msg.From.ID, state, 30*time.Minute)
		b.finishAddProduct(chatID, msg.From.ID, state.CategoryID, lang)
	}
	return true
}

func (b *Bot) finishAddProduct(chatID, userID, categoryID int64, lang string) {
	ctx := context.Background()
	state, _ := b.fsm.GetAddProductState(ctx, userID)
	_ = b.fsm.DelAddProductState(ctx, userID)
	if state == nil {
		return
	}
	cover := ""
	if len(state.Photos) > 0 {
		cover = state.Photos[0]
	}
	p := &storage.Product{CategoryID: categoryID, Name: state.Name, Description: state.Description, PriceUSD: state.PriceUSD, Stock: state.Stock, PhotoURL: cover, IsActive: true, SubPeriodDays: state.SubPeriodDays}
	id, err := b.products.CreateProduct(ctx, p)
	if err != nil {
		b.logger.Error("create product", "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_product_create_failed")))
		return
	}
	for _, fileID := range state.Photos {
		if err := b.photos.Add(ctx, id, fileID); err != nil {
			b.logger.Error("add product photo", "product_id", id, "error", err)
		}
	}
	b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_product_created")))
}

// handleWizardPhotoStep processes StepPhoto: a photo message (largest
// PhotoSize wins) or a URL adds an image, up to storage.MaxProductPhotos;
// /done (or /skip) finishes the step. When state.EditProductID is set the
// photo is persisted directly on that product instead of the wizard state.
func (b *Bot) handleWizardPhotoStep(ctx context.Context, msg *tgbotapi.Message, state *storage.AddProductState) {
	chatID := msg.Chat.ID
	lang := msg.From.LanguageCode

	switch msg.Command() {
	case "done", "skip":
		if state.EditProductID != 0 {
			_ = b.fsm.DelAddProductState(ctx, msg.From.ID)
			b.sendAdminPhotoList(chatID, 0, state.EditProductID, lang)
			return
		}
		state.Step = storage.StepCategory
		_ = b.fsm.SetAddProductState(ctx, msg.From.ID, state, 30*time.Minute)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_add_product_category")))
		return
	}

	fileID := ""
	if len(msg.Photo) > 0 {
		fileID = largestPhotoFileID(msg.Photo)
	} else if text := strings.TrimSpace(msg.Text); text != "" {
		fileID = text
	}
	if fileID == "" {
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_photo_prompt")))
		return
	}

	if state.EditProductID != 0 {
		b.addProductPhoto(ctx, chatID, state.EditProductID, fileID, lang)
		return
	}

	if !appendWizardPhoto(state, fileID) {
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_photo_limit")))
		return
	}
	_ = b.fsm.SetAddProductState(ctx, msg.From.ID, state, 30*time.Minute)
	b.send(tgbotapi.NewMessage(chatID, b.i18n.Tf(lang, "admin_photo_more", len(state.Photos), storage.MaxProductPhotos)))
}

// largestPhotoFileID returns the FileID of the PhotoSize with the largest
// pixel area. Telegram usually sends sizes sorted ascending, but the order is
// not guaranteed, so pick the maximum explicitly.
func largestPhotoFileID(sizes []tgbotapi.PhotoSize) string {
	fileID := ""
	bestArea := -1
	for _, s := range sizes {
		if area := s.Width * s.Height; area > bestArea {
			bestArea = area
			fileID = s.FileID
		}
	}
	return fileID
}

// appendWizardPhoto adds fileID to the in-progress wizard state and reports
// whether it fit under the storage.MaxProductPhotos limit.
func appendWizardPhoto(state *storage.AddProductState, fileID string) bool {
	if len(state.Photos) >= storage.MaxProductPhotos {
		return false
	}
	state.Photos = append(state.Photos, fileID)
	return true
}

// addProductPhoto persists one gallery photo on an existing product and sets
// it as the cover when the product has none.
func (b *Bot) addProductPhoto(ctx context.Context, chatID, productID int64, fileID, lang string) {
	if err := b.photos.Add(ctx, productID, fileID); err != nil {
		if errors.Is(err, storage.ErrTooManyPhotos) {
			b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_photo_limit")))
			return
		}
		b.logger.Error("add product photo", "product_id", productID, "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_photo_error")))
		return
	}
	if p, err := b.products.GetProduct(ctx, productID); err == nil && p.PhotoURL == "" {
		p.PhotoURL = fileID
		if err := b.products.UpdateProduct(ctx, p); err != nil {
			b.logger.Warn("update product cover", "product_id", productID, "error", err)
		}
	}
	b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_photo_added")))
}

// sendAdminPhotoList renders the photo management screen for a product:
// one delete button per photo plus an add button.
func (b *Bot) sendAdminPhotoList(chatID int64, msgID int, productID int64, lang string) {
	ctx := context.Background()
	photos, err := b.photos.List(ctx, productID)
	if err != nil {
		b.logger.Error("list product photos", "product_id", productID, "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_photo_error")))
		return
	}

	text := b.i18n.Tf(lang, "admin_photo_list_title", productID, len(photos), storage.MaxProductPhotos)
	if len(photos) == 0 {
		text += "\n" + b.t(lang, "admin_photo_none")
	}

	kb := make(StyledKeyboard, 0, len(photos)+1)
	for i, ph := range photos {
		kb = append(kb, []StyledButton{BtnDanger(fmt.Sprintf("🗑 %d", i+1), fmt.Sprintf("admin:photodel:%d:%d", ph.ID, productID))})
	}
	if len(photos) < storage.MaxProductPhotos {
		kb = append(kb, []StyledButton{Btn(b.t(lang, "admin_photo_add_btn"), fmt.Sprintf("admin:photoadd:%d", productID))})
	}
	b.sendOrEditStyled(chatID, msgID, text, "", kb)
}

// onAdminPhotoDelete handles admin:photodel:<photoID>:<productID>. After
// deleting it re-syncs the cover for wizard-managed (file_id) covers and
// re-renders the list.
func (b *Bot) onAdminPhotoDelete(chatID int64, msgID int, data, lang string) {
	parts := strings.Split(strings.TrimPrefix(data, "admin:photodel:"), ":")
	if len(parts) != 2 {
		return
	}
	photoID, err1 := strconv.ParseInt(parts[0], 10, 64)
	productID, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return
	}

	ctx := context.Background()
	if err := b.photos.Delete(ctx, photoID); err != nil {
		b.logger.Error("delete product photo", "photo_id", photoID, "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_photo_error")))
		return
	}
	b.syncProductCover(ctx, productID)
	b.sendAdminPhotoList(chatID, msgID, productID, lang)
}

// syncProductCover keeps products.photo_url pointing at an existing gallery
// photo. Explicit http(s) URL covers set by the admin are left untouched.
func (b *Bot) syncProductCover(ctx context.Context, productID int64) {
	p, err := b.products.GetProduct(ctx, productID)
	if err != nil {
		b.logger.Warn("get product for cover sync", "product_id", productID, "error", err)
		return
	}
	if strings.HasPrefix(p.PhotoURL, "http://") || strings.HasPrefix(p.PhotoURL, "https://") {
		return
	}
	photos, err := b.photos.List(ctx, productID)
	if err != nil {
		b.logger.Warn("list photos for cover sync", "product_id", productID, "error", err)
		return
	}
	for _, ph := range photos {
		if ph.FileID == p.PhotoURL {
			return
		}
	}
	cover := ""
	if len(photos) > 0 {
		cover = photos[0].FileID
	}
	if p.PhotoURL == cover {
		return
	}
	p.PhotoURL = cover
	if err := b.products.UpdateProduct(ctx, p); err != nil {
		b.logger.Warn("update product cover", "product_id", productID, "error", err)
	}
}

// onAdminPhotoAdd handles admin:photoadd:<productID>: puts the admin into a
// photo-only wizard state bound to the existing product.
func (b *Bot) onAdminPhotoAdd(chatID, userID int64, data, lang string) {
	productID, err := parseIDFromCallback(data, "admin:photoadd:")
	if err != nil {
		b.logger.Error("parse admin:photoadd callback", "error", err)
		return
	}
	ctx := context.Background()
	_ = b.fsm.SetAddProductState(ctx, userID, &storage.AddProductState{Step: storage.StepPhoto, EditProductID: productID, CreatedAt: time.Now()}, 30*time.Minute)
	b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_photo_prompt")))
}

func (b *Bot) sendAdminProductDetails(chatID int64, product *storage.Product, lang string) {
	toggleLabel := b.t(lang, "admin_btn_stock_off")
	if product.Stock <= 0 {
		toggleLabel = b.t(lang, "admin_btn_stock_on")
	}

	text := fmt.Sprintf(
		b.t(lang, "admin_product_details"),
		product.ID, product.Name, product.Description, product.PriceUSD, product.Stock, product.CategoryID, product.IsActive,
		product.ID, product.ID, product.ID, product.ID, product.ID, product.ID,
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(toggleLabel, fmt.Sprintf("admin:togglestock:%d", product.ID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(b.t(lang, "admin_photo_btn"), fmt.Sprintf("admin:photos:%d", product.ID)),
		),
	)

	reply := tgbotapi.NewMessage(chatID, text)
	reply.ReplyMarkup = keyboard
	b.send(reply)
}

func (b *Bot) handleEditProduct(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode
	id, err := strconv.ParseInt(strings.TrimSpace(msg.CommandArguments()), 10, 64)
	if err != nil {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_usage_editproduct")))
		return
	}

	product, err := b.products.GetProduct(context.Background(), id)
	if err != nil {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_product_not_found")))
		return
	}

	b.sendAdminProductDetails(msg.Chat.ID, product, lang)
}

func (b *Bot) handleEditProductField(msg *tgbotapi.Message, prodID int64, field, value string) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode
	if strings.TrimSpace(value) == "" {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_field_value_required")))
		return
	}

	ctx := context.Background()
	product, err := b.products.GetProduct(ctx, prodID)
	if err != nil {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_product_not_found")))
		return
	}

	switch strings.ToLower(field) {
	case "name":
		product.Name = value
	case "description":
		product.Description = value
	case "price":
		price, err := strconv.ParseFloat(value, 64)
		if err != nil {
			b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_invalid_price")))
			return
		}
		product.PriceUSD = price
	case "stock":
		stock, err := strconv.Atoi(value)
		if err != nil {
			b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_invalid_stock")))
			return
		}
		product.Stock = stock
	case "category":
		categoryID, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_invalid_category_id")))
			return
		}
		product.CategoryID = categoryID
	case "active":
		active, err := strconv.ParseBool(value)
		if err != nil {
			b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_invalid_bool")))
			return
		}
		product.IsActive = active
	default:
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_supported_fields")))
		return
	}

	if err := b.products.UpdateProduct(ctx, product); err != nil {
		b.logger.Error("update product", "product_id", prodID, "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_product_update_failed")))
		return
	}

	b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_product_updated")))
}

func (b *Bot) handleDeleteProduct(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode
	id, err := strconv.ParseInt(strings.TrimSpace(msg.CommandArguments()), 10, 64)
	if err != nil {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_usage_deleteproduct")))
		return
	}
	if err := b.products.DeleteProduct(context.Background(), id); err != nil {
		b.logger.Error("delete product", "product_id", id, "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_product_delete_failed")))
		return
	}
	b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_product_deleted")))
}

func (b *Bot) handleAddCategory(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode
	args := strings.Fields(msg.CommandArguments())
	if len(args) < 2 {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_usage_addcategory")))
		return
	}
	cat := &storage.Category{
		Emoji:    args[0],
		Name:     strings.Join(args[1:], " "),
		IsActive: true,
	}
	id, err := b.catalog.CreateCategory(context.Background(), cat)
	if err != nil {
		b.logger.Error("create category", "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_category_create_failed")))
		return
	}
	b.send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf(b.t(lang, "admin_category_created"), id, cat.Emoji, cat.Name)))
}

func (b *Bot) handleEditCategory(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode
	args := strings.Fields(msg.CommandArguments())
	if len(args) < 3 {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_usage_editcategory")))
		return
	}

	categoryID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_invalid_category_id")))
		return
	}

	ctx := context.Background()
	category, err := b.catalog.GetCategory(ctx, categoryID)
	if err != nil {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_category_not_found")))
		return
	}

	value := strings.Join(args[2:], " ")
	switch strings.ToLower(args[1]) {
	case "name":
		category.Name = value
	case "emoji":
		category.Emoji = value
	default:
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_category_fields")))
		return
	}

	if err := b.catalog.UpdateCategory(ctx, category); err != nil {
		b.logger.Error("update category", "category_id", categoryID, "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_category_update_failed")))
		return
	}

	b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_category_updated")))
}

func (b *Bot) handleDeleteCategory(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode
	id, err := strconv.ParseInt(strings.TrimSpace(msg.CommandArguments()), 10, 64)
	if err != nil {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_usage_deletecategory")))
		return
	}
	if err := b.catalog.DeleteCategory(context.Background(), id); err != nil {
		b.logger.Error("delete category", "category_id", id, "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_category_delete_failed")))
		return
	}
	b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_category_deleted")))
}

func (b *Bot) handleListCategories(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode
	categories, err := b.catalog.ListCategories(context.Background())
	if err != nil {
		b.logger.Error("list categories", "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_categories_load_failed")))
		return
	}
	if len(categories) == 0 {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_categories_empty")))
		return
	}

	var sb strings.Builder
	sb.WriteString(b.t(lang, "admin_categories_title"))
	for _, category := range categories {
		sb.WriteString(fmt.Sprintf("%d: %s %s\n", category.ID, category.Emoji, category.Name))
	}
	b.send(tgbotapi.NewMessage(msg.Chat.ID, sb.String()))
}

func (b *Bot) handleOrdersAll(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode
	statusFilter := strings.TrimSpace(msg.CommandArguments())
	orders, err := b.order.GetAllOrders(context.Background(), statusFilter)
	if err != nil {
		b.logger.Error("get all orders", "status", statusFilter, "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_orders_load_failed")))
		return
	}
	if len(orders) == 0 {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_orders_empty")))
		return
	}

	var sb strings.Builder
	sb.WriteString(b.t(lang, "admin_orders_title"))
	for _, order := range orders {
		status := storage.StatusDisplay[order.Status]
		if status == "" {
			status = order.Status
		}
		sb.WriteString(fmt.Sprintf("#%d | user %d | $%.2f / %d ⭐ | %s\n",
			order.ID, order.UserID, order.TotalUSD, order.TotalStars, status))
	}
	b.send(tgbotapi.NewMessage(msg.Chat.ID, sb.String()))
}

func (b *Bot) handleSetDelivered(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode
	id, err := strconv.ParseInt(strings.TrimSpace(msg.CommandArguments()), 10, 64)
	if err != nil {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_usage_setdelivered")))
		return
	}
	order, err := b.order.SetDelivered(context.Background(), id)
	if err != nil {
		b.logger.Error("set delivered", "order_id", id, "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_set_delivered_failed")))
		return
	}
	b.send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf(b.t(lang, "admin_delivered_ok"), order.ID)))

	// Invite the buyer to rate the freshly delivered order (1..5 stars).
	b.sendReviewInvite(context.Background(), order)

	b.notifyAdmins(context.Background(), AdminEventOrderDelivered,
		fmt.Sprintf(b.t("en", "admin_order_delivered"), order.ID, order.UserID))

	b.outWebhook.Send(service.OutboundWebhookEvent{
		Event:      "order.delivered",
		OrderID:    order.ID,
		UserID:     order.UserID,
		TotalUSD:   order.TotalUSD,
		TotalStars: order.TotalStars,
	})
}

func (b *Bot) handleAnalytics(msg *tgbotapi.Message) {
	b.sendAnalytics(msg.Chat.ID, 0, analyticsDefaultDays, msg.From.LanguageCode)
}

func (b *Bot) handleAnalyticsCallback(chatID int64, msgID int, data, lang string) {
	days := analyticsDefaultDays
	if strings.HasPrefix(data, "analytics:") {
		if parsed, err := strconv.Atoi(strings.TrimPrefix(data, "analytics:")); err == nil && parsed > 0 {
			days = parsed
		}
	}
	b.sendAnalytics(chatID, msgID, days, lang)
}

const (
	// analyticsDefaultDays is the default reporting window: the spec asks for
	// a 14-day revenue chart on /analytics.
	analyticsDefaultDays = 14
	// revenueChartWidth is the bar length of the busiest day, in ▇ blocks.
	revenueChartWidth = 10
)

// renderRevenueChart renders one text-bar line per day for the `days` days
// ending at `today` (inclusive, oldest first). Bars are normalized so the
// busiest day spans revenueChartWidth ▇ blocks; days without revenue render
// as a single "·".
func renderRevenueChart(daily []storage.DailyRevenue, today time.Time, days int) string {
	byDate := make(map[string]float64, len(daily))
	var maxUSD float64
	for _, d := range daily {
		byDate[d.Date] = d.TotalUSD
		if d.TotalUSD > maxUSD {
			maxUSD = d.TotalUSD
		}
	}

	var sb strings.Builder
	for i := days - 1; i >= 0; i-- {
		day := today.AddDate(0, 0, -i)
		usd := byDate[day.Format("2006-01-02")]
		sb.WriteString(day.Format("01-02"))
		sb.WriteByte(' ')
		if usd <= 0 || maxUSD <= 0 {
			sb.WriteString("·")
		} else {
			bars := int(math.Round(usd / maxUSD * revenueChartWidth))
			if bars < 1 {
				bars = 1
			}
			sb.WriteString(strings.Repeat("▇", bars))
			fmt.Fprintf(&sb, " $%.2f", usd)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func (b *Bot) sendAnalytics(chatID int64, msgID int, days int, lang string) {
	ctx := context.Background()
	fail := func(stage string, err error) {
		b.logger.Error("analytics "+stage, "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_analytics_error")))
	}

	summary, err := b.analytics.GetRevenueSummary(ctx)
	if err != nil {
		fail("summary", err)
		return
	}
	revenueByDays, err := b.analytics.GetRevenueByDays(ctx, days)
	if err != nil {
		fail("revenue by days", err)
		return
	}
	topProducts, err := b.analytics.GetTopProducts(ctx, 5)
	if err != nil {
		fail("top products", err)
		return
	}
	topBuyers, err := b.analytics.TopBuyers(ctx, 10)
	if err != nil {
		fail("top buyers", err)
		return
	}
	promoUsage, err := b.analytics.PromoUsage(ctx)
	if err != nil {
		fail("promo usage", err)
		return
	}
	paymentStats, err := b.analytics.GetPaymentMethodStats(ctx)
	if err != nil {
		fail("payment stats", err)
		return
	}

	none := b.t(lang, "admin_analytics_none") + "\n"

	var sb strings.Builder
	sb.WriteString(b.i18n.Tf(lang, "admin_analytics_title", days) + "\n\n")
	sb.WriteString(b.i18n.Tf(lang, "admin_analytics_total_orders", summary.TotalOrders) + "\n")
	sb.WriteString(b.i18n.Tf(lang, "admin_analytics_paid_orders", summary.PaidOrders) + "\n")

	var periodUSD float64
	var periodStars int
	var periodOrders int
	for _, day := range revenueByDays {
		periodUSD += day.TotalUSD
		periodStars += day.TotalStars
		periodOrders += day.OrderCount
	}
	sb.WriteString(b.i18n.Tf(lang, "admin_analytics_period_revenue", periodUSD, periodStars, periodOrders) + "\n\n")

	sb.WriteString(b.t(lang, "admin_analytics_chart_title") + "\n")
	sb.WriteString(renderRevenueChart(revenueByDays, time.Now().UTC(), days))
	sb.WriteString("\n")

	sb.WriteString(b.t(lang, "admin_analytics_top_products") + "\n")
	if len(topProducts) == 0 {
		sb.WriteString(none)
	} else {
		for _, product := range topProducts {
			sb.WriteString(b.i18n.Tf(lang, "admin_analytics_product_row", product.Name, product.TotalSold, product.TotalRevenue) + "\n")
		}
	}

	sb.WriteString("\n" + b.t(lang, "admin_analytics_top_buyers") + "\n")
	if len(topBuyers) == 0 {
		sb.WriteString(none)
	} else {
		for i, buyer := range topBuyers {
			sb.WriteString(b.i18n.Tf(lang, "admin_analytics_buyer_row", i+1, buyer.UserID, buyer.Orders, buyer.TotalUSD) + "\n")
		}
	}

	sb.WriteString("\n" + b.t(lang, "admin_analytics_promo_title") + "\n")
	if len(promoUsage) == 0 {
		sb.WriteString(none)
	} else {
		for _, promo := range promoUsage {
			if promo.DiscountKnown {
				sb.WriteString(b.i18n.Tf(lang, "admin_analytics_promo_row", promo.Code, promo.Uses, promo.DiscountUSD) + "\n")
			} else {
				sb.WriteString(b.i18n.Tf(lang, "admin_analytics_promo_row_unknown", promo.Code, promo.Uses) + "\n")
			}
		}
	}

	sb.WriteString("\n" + b.t(lang, "admin_analytics_payments_title") + "\n")
	if len(paymentStats) == 0 {
		sb.WriteString(none)
	} else {
		for _, stat := range paymentStats {
			sb.WriteString(b.i18n.Tf(lang, "admin_analytics_payment_row", stat.Method, stat.OrderCount, stat.TotalUSD) + "\n")
		}
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(b.i18n.Tf(lang, "admin_analytics_btn_days", 7), "analytics:7"),
			tgbotapi.NewInlineKeyboardButtonData(b.i18n.Tf(lang, "admin_analytics_btn_days", 14), "analytics:14"),
			tgbotapi.NewInlineKeyboardButtonData(b.i18n.Tf(lang, "admin_analytics_btn_days", 30), "analytics:30"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Admin Menu", "admin:menu"),
		),
	)

	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, sb.String())
		edit.ReplyMarkup = &keyboard
		b.send(edit)
		return
	}

	reply := tgbotapi.NewMessage(chatID, sb.String())
	reply.ReplyMarkup = keyboard
	b.send(reply)
}
func (b *Bot) handleAddPromo(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	args := strings.Fields(msg.CommandArguments())
	if len(args) < 2 {
		return
	}
	discount, _ := strconv.Atoi(args[1])
	p := &storage.PromoCode{Code: strings.ToUpper(args[0]), Discount: discount, IsActive: true}
	_, _ = b.promos.CreatePromo(context.Background(), p)
	b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(msg.From.LanguageCode, "admin_promo_created")))
}

func (b *Bot) handleListPromos(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	promos, _ := b.promos.ListPromos(context.Background())
	var sb strings.Builder
	for _, p := range promos {
		sb.WriteString(fmt.Sprintf("%d: %s (-%d%%)\n", p.ID, p.Code, p.Discount))
	}
	b.send(tgbotapi.NewMessage(msg.Chat.ID, sb.String()))
}

func (b *Bot) handleDeletePromo(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	id, _ := strconv.ParseInt(msg.CommandArguments(), 10, 64)
	_ = b.promos.DeactivatePromo(context.Background(), id)
	b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(msg.From.LanguageCode, "admin_promo_deactivated")))
}

// exportDateLayout is the accepted /export_orders argument format.
const exportDateLayout = "2006-01-02"

// parseExportRange parses the optional [from] [to] arguments of
// /export_orders. Nil means "unbounded". The returned `to` bound is
// exclusive: it points at midnight AFTER the requested inclusive end date.
// A malformed argument is returned verbatim in bad.
func parseExportRange(args []string) (from, to *time.Time, bad string) {
	if len(args) > 0 {
		t, err := time.Parse(exportDateLayout, args[0])
		if err != nil {
			return nil, nil, args[0]
		}
		from = &t
	}
	if len(args) > 1 {
		t, err := time.Parse(exportDateLayout, args[1])
		if err != nil {
			return nil, nil, args[1]
		}
		end := t.AddDate(0, 0, 1)
		to = &end
	}
	return from, to, ""
}

// filterOrdersByDate keeps orders with from <= CreatedAt < to. Nil bounds are
// unbounded, so (nil, nil) returns the input unchanged.
func filterOrdersByDate(orders []storage.Order, from, to *time.Time) []storage.Order {
	if from == nil && to == nil {
		return orders
	}
	filtered := make([]storage.Order, 0, len(orders))
	for _, order := range orders {
		if from != nil && order.CreatedAt.Before(*from) {
			continue
		}
		if to != nil && !order.CreatedAt.Before(*to) {
			continue
		}
		filtered = append(filtered, order)
	}
	return filtered
}

func (b *Bot) handleExportOrders(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	lang := msg.From.LanguageCode

	from, to, bad := parseExportRange(strings.Fields(msg.CommandArguments()))
	if bad != "" {
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.i18n.Tf(lang, "admin_export_bad_date", bad)))
		return
	}

	orders, err := b.order.GetAllOrders(context.Background(), "")
	if err != nil {
		b.logger.Error("export orders", "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_export_failed")))
		return
	}
	orders = filterOrdersByDate(orders, from, to)

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	_ = writer.Write([]string{
		"order_id",
		"user_id",
		"status",
		"total_usd",
		"total_stars",
		"payment_method",
		"payment_id",
		"discount_pct",
		"promo_code",
		"created_at",
	})

	for _, order := range orders {
		_ = writer.Write([]string{
			strconv.FormatInt(order.ID, 10),
			strconv.FormatInt(order.UserID, 10),
			order.Status,
			fmt.Sprintf("%.2f", order.TotalUSD),
			strconv.Itoa(order.TotalStars),
			order.PaymentMethod,
			order.PaymentID,
			strconv.Itoa(order.DiscountPct),
			order.PromoCode,
			order.CreatedAt.Format(time.RFC3339),
		})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		b.logger.Error("flush order export csv", "error", err)
		b.send(tgbotapi.NewMessage(msg.Chat.ID, b.t(lang, "admin_export_failed")))
		return
	}

	doc := tgbotapi.NewDocument(msg.Chat.ID, tgbotapi.FileBytes{
		Name:  fmt.Sprintf("orders_%s.csv", time.Now().Format("20060102_150405")),
		Bytes: buf.Bytes(),
	})
	doc.Caption = b.i18n.Tf(lang, "admin_export_caption", len(orders))
	b.send(doc)
}

func (b *Bot) onAdminToggleStock(chatID int64, data, lang string) {
	productID, err := parseIDFromCallback(data, "admin:togglestock:")
	if err != nil {
		b.logger.Error("parse admin:togglestock callback", "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_product_parse_failed")))
		return
	}

	ctx := context.Background()
	product, err := b.products.GetProduct(ctx, productID)
	if err != nil {
		b.logger.Error("get product for stock toggle", "product_id", productID, "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_product_not_found")))
		return
	}

	if product.Stock > 0 {
		product.Stock = 0
	} else {
		product.Stock = 1
		product.IsActive = true
	}

	if err := b.products.UpdateProduct(ctx, product); err != nil {
		b.logger.Error("toggle product stock", "product_id", productID, "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_product_update_failed")))
		return
	}

	b.sendAdminProductDetails(chatID, product, lang)
}

func (b *Bot) routeEditProduct(msg *tgbotapi.Message) {
	args := strings.Fields(msg.CommandArguments())
	if len(args) == 0 {
		return
	}
	id, _ := strconv.ParseInt(args[0], 10, 64)
	if len(args) == 1 {
		b.handleEditProduct(msg)
	} else {
		b.handleEditProductField(msg, id, args[1], strings.Join(args[2:], " "))
	}
}

// handleBtnStyleAdmin handles the /btnstyle command.
func (b *Bot) handleBtnStyleAdmin(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		return
	}
	b.sendBtnStyleList(msg.Chat.ID, 0, msg.From.LanguageCode)
}

// sendBtnStyleList renders (or edits) the button style overview for the admin.
// Each row shows the button label and its current style emoji, tapping opens the picker.
func (b *Bot) sendBtnStyleList(chatID int64, msgID int, lang string) {
	ctx := context.Background()
	stored, _ := b.uiSettings.ListButtonStyles(ctx)

	var sb strings.Builder
	sb.WriteString(b.t(lang, "admin_btnstyle_title"))
	for _, key := range AllButtonKeys {
		style := ButtonStyle(stored[key])
		sb.WriteString(fmt.Sprintf("%s %s → %s\n", StyleEmoji(style), ButtonKeyLabel(key), styleLabel(style)))
	}

	// Build inline keyboard: 2 buttons per row (label + current style indicator).
	var rows [][]StyledButton
	for i := 0; i < len(AllButtonKeys); i += 2 {
		var row []StyledButton
		for j := i; j < i+2 && j < len(AllButtonKeys); j++ {
			key := AllButtonKeys[j]
			style := ButtonStyle(stored[key])
			label := fmt.Sprintf("%s %s", StyleEmoji(style), ButtonKeyLabel(key))
			row = append(row, Btn(label, "admin:btnpick:"+key))
		}
		rows = append(rows, row)
	}
	rows = append(rows, []StyledButton{Btn("◀️ Admin Menu", "admin:menu")})

	kb := StyledKeyboard(rows)
	b.sendOrEditStyled(chatID, msgID, sb.String(), "HTML", kb)
}

// sendBtnStylePicker renders (or edits) the style picker for a single button key.
func (b *Bot) sendBtnStylePicker(chatID int64, msgID int, key string, lang string) {
	ctx := context.Background()
	current, _ := b.uiSettings.GetButtonStyle(ctx, key)

	text := fmt.Sprintf(
		b.t(lang, "admin_btnstyle_picker"),
		ButtonKeyLabel(key),
		StyleEmoji(ButtonStyle(current)),
		styleLabel(ButtonStyle(current)),
	)

	styleOptions := []struct {
		label string
		style ButtonStyle
	}{
		{"🔵 Primary", StylePrimary},
		{"🟢 Success", StyleSuccess},
		{"🔴 Danger", StyleDanger},
		{b.t(lang, "admin_btnstyle_default"), StyleDefault},
	}

	var styleRow []StyledButton
	for _, opt := range styleOptions {
		styleRow = append(styleRow, Btn(opt.label, fmt.Sprintf("admin:setstyle:%s:%s", key, string(opt.style))))
	}

	kb := StyledKeyboard{
		styleRow,
		{Btn(b.t(lang, "admin_btnstyle_back"), "admin:btnlist")},
	}
	b.sendOrEditStyled(chatID, msgID, text, "HTML", kb)
}

// onAdminSetStyle persists the style choice and returns to the overview.
func (b *Bot) onAdminSetStyle(chatID int64, msgID int, data, lang string) {
	// data format: "admin:setstyle:<key>:<style>"
	rest := strings.TrimPrefix(data, "admin:setstyle:")
	sep := strings.LastIndex(rest, ":")
	if sep < 0 {
		return
	}
	key := rest[:sep]
	style := rest[sep+1:]

	ctx := context.Background()
	if err := b.uiSettings.SetButtonStyle(ctx, key, style); err != nil {
		b.logger.Error("set button style", "key", key, "style", style, "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_btnstyle_save_failed")))
		return
	}
	// Invalidate cache entry and reload.
	b.uiStyles.Store(key, style)

	b.sendBtnStyleList(chatID, msgID, lang)
}

func styleLabel(s ButtonStyle) string {
	switch s {
	case StylePrimary:
		return "primary"
	case StyleSuccess:
		return "success"
	case StyleDanger:
		return "danger"
	default:
		return "default"
	}
}

// -------------------------------------------------------------
// Interactive 100% Inline Button Admin UI
// -------------------------------------------------------------

func (b *Bot) startAddProductWizard(chatID, userID int64, lang string) {
	ctx := context.Background()
	_ = b.fsm.SetAddProductState(ctx, userID, &storage.AddProductState{Step: storage.StepName, CreatedAt: time.Now()}, 30*time.Minute)
	b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_add_product_name")))
}

// sendAdminMenu renders the interactive admin dashboard with 100% inline buttons.
func (b *Bot) sendAdminMenu(chatID int64, msgID int, lang string) {
	text := "🛠 <b>Admin Dashboard</b>\n\nManage products, categories, orders, promo codes, and analytics completely from here:"
	kb := tgbotapi.NewInlineKeyboardMarkup(
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
	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.send(edit)
		return
	}
	reply := tgbotapi.NewMessage(chatID, text)
	reply.ParseMode = "HTML"
	reply.ReplyMarkup = kb
	b.send(reply)
}

func (b *Bot) sendAdminCategories(chatID int64, msgID int, lang string) {
	ctx := context.Background()
	categories, err := b.products.GetCategories(ctx)
	if err != nil {
		b.logger.Error("admin categories: list", "error", err)
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	text := "📂 <b>Categories Management</b>\n\nTap a category to view/delete it, or add a new one:"
	if len(categories) == 0 {
		text += "\n\n<i>No categories created yet.</i>"
	} else {
		for _, cat := range categories {
			btnText := fmt.Sprintf("%s %s", cat.Emoji, cat.Name)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(btnText, fmt.Sprintf("admin:cat:view:%d", cat.ID)),
			))
		}
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("➕ Add Category", "admin:cat:add"),
	))
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ Admin Menu", "admin:menu"),
	))

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.send(edit)
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = kb
	b.send(msg)
}

func (b *Bot) sendAdminCategoryView(chatID int64, msgID int, catID int64, lang string) {
	ctx := context.Background()
	cat, err := b.products.GetCategory(ctx, catID)
	if err != nil {
		b.logger.Error("admin category view: get", "error", err)
		return
	}
	products, _ := b.products.GetProductsByCategory(ctx, catID)

	text := fmt.Sprintf("📂 <b>Category #%d</b>\n\nName: %s %s\nTotal Products: %d", cat.ID, cat.Emoji, cat.Name, len(products))

	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("🛍 View Products (%d)", len(products)), fmt.Sprintf("admin:prod:cat:%d", cat.ID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗑 Delete Category", fmt.Sprintf("admin:cat:del:%d", cat.ID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Back to Categories", "admin:categories"),
		),
	)

	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.send(edit)
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = kb
	b.send(msg)
}

func (b *Bot) onAdminCategoryDelete(cbID string, chatID int64, msgID int, catID int64, lang string) {
	ctx := context.Background()
	if err := b.products.DeleteCategory(ctx, catID); err != nil {
		b.logger.Error("admin delete category", "cat_id", catID, "error", err)
		b.alert(cbID, b.t(lang, "admin_category_delete_failed"))
		return
	}
	b.alert(cbID, b.t(lang, "admin_category_deleted"))
	b.sendAdminCategories(chatID, msgID, lang)
}

func (b *Bot) onAdminCategoryAddPrompt(chatID, userID int64, lang string) {
	b.adminActions.Store(userID, "add_category")
	msg := tgbotapi.NewMessage(chatID, "📂 <b>Add Category</b>\n\nPlease send the category emoji and name (e.g., <code>👟 Shoes</code> or <code>📱 Phones</code>).\n\nSend /cancel to abort.")
	msg.ParseMode = "HTML"
	b.send(msg)
}

func (b *Bot) sendAdminProductCategories(chatID int64, msgID int, lang string) {
	ctx := context.Background()
	categories, err := b.products.GetCategories(ctx)
	if err != nil {
		b.logger.Error("admin product categories: list", "error", err)
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	text := "🛍 <b>Products Management</b>\n\nSelect a category to view and manage products:"
	for _, cat := range categories {
		btnText := fmt.Sprintf("%s %s", cat.Emoji, cat.Name)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(btnText, fmt.Sprintf("admin:prod:cat:%d", cat.ID)),
		))
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("➕ Add New Product", "admin:prod:add"),
	))
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ Admin Menu", "admin:menu"),
	))

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.send(edit)
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = kb
	b.send(msg)
}

func (b *Bot) sendAdminCategoryProducts(chatID int64, msgID int, catID int64, lang string) {
	ctx := context.Background()
	cat, err := b.products.GetCategory(ctx, catID)
	if err != nil {
		b.logger.Error("admin category products: get category", "error", err)
		return
	}
	products, _ := b.products.GetProductsByCategory(ctx, catID)

	text := fmt.Sprintf("🛍 <b>Products in %s %s</b> (%d items):", cat.Emoji, cat.Name, len(products))
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, p := range products {
		stockStatus := fmt.Sprintf("(%d in stock)", p.Stock)
		if !p.IsActive || p.Stock <= 0 {
			stockStatus = "(Out of Stock)"
		}
		label := fmt.Sprintf("• %s - $%.2f %s", p.Name, p.PriceUSD, stockStatus)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("admin:prod:view:%d", p.ID)),
		))
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("➕ Add Product to this Category", "admin:prod:add"),
	))
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ Back to Categories", "admin:products:cats"),
	))

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.send(edit)
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = kb
	b.send(msg)
}

func (b *Bot) sendAdminProductView(chatID int64, msgID int, prodID int64, lang string) {
	ctx := context.Background()
	p, err := b.products.GetProduct(ctx, prodID)
	if err != nil {
		b.logger.Error("admin product view: get", "prod_id", prodID, "error", err)
		return
	}

	statusText := "Active"
	if !p.IsActive {
		statusText = "Inactive"
	}
	subText := "None"
	if p.SubPeriodDays > 0 {
		subText = fmt.Sprintf("%d days (Recurring)", p.SubPeriodDays)
	}

	text := fmt.Sprintf("📦 <b>Product #%d</b>\n\n"+
		"<b>Name:</b> %s\n"+
		"<b>Description:</b> %s\n"+
		"<b>Price:</b> $%.2f / %d ⭐\n"+
		"<b>Stock:</b> %d pcs\n"+
		"<b>Status:</b> %s\n"+
		"<b>Subscription:</b> %s",
		p.ID, p.Name, p.Description, p.PriceUSD, p.PriceStars, p.Stock, statusText, subText)

	toggleLabel := b.t(lang, "admin_btn_stock_off")
	if !p.IsActive || p.Stock == 0 {
		toggleLabel = b.t(lang, "admin_btn_stock_on")
	}

	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(toggleLabel, fmt.Sprintf("admin:togglestock:%d", p.ID)),
			tgbotapi.NewInlineKeyboardButtonData(b.t(lang, "admin_photo_btn"), fmt.Sprintf("admin:photos:%d", p.ID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗑 Delete Product", fmt.Sprintf("admin:prod:del:%d", p.ID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Back to Category", fmt.Sprintf("admin:prod:cat:%d", p.CategoryID)),
		),
	)

	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.send(edit)
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = kb
	b.send(msg)
}

func (b *Bot) onAdminProductDelete(cbID string, chatID int64, msgID int, prodID int64, lang string) {
	ctx := context.Background()
	p, err := b.products.GetProduct(ctx, prodID)
	catID := int64(0)
	if err == nil && p != nil {
		catID = p.CategoryID
	}
	if err := b.products.DeleteProduct(ctx, prodID); err != nil {
		b.logger.Error("admin delete product", "prod_id", prodID, "error", err)
		b.alert(cbID, b.t(lang, "admin_product_delete_failed"))
		return
	}
	b.alert(cbID, b.t(lang, "admin_product_deleted"))
	if catID > 0 {
		b.sendAdminCategoryProducts(chatID, msgID, catID, lang)
	} else {
		b.sendAdminProductCategories(chatID, msgID, lang)
	}
}

func (b *Bot) sendAdminOrders(chatID int64, msgID int, filter string, lang string) {
	ctx := context.Background()
	statusFilter := ""
	if filter != "all" {
		statusFilter = filter
	}

	orders, err := b.order.GetAllOrders(ctx, statusFilter)
	if err != nil {
		b.logger.Error("admin orders: get all", "filter", filter, "error", err)
		return
	}

	allBtn := "All"
	pendingBtn := "⏳ Pending"
	paidBtn := "💳 Paid"
	deliveredBtn := "🚚 Delivered"
	switch filter {
	case "all":
		allBtn = "• All •"
	case "pending":
		pendingBtn = "• ⏳ Pending •"
	case "paid":
		paidBtn = "• 💳 Paid •"
	case "delivered":
		deliveredBtn = "• 🚚 Delivered •"
	}

	filterRow := tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData(allBtn, "admin:orders:all"),
		tgbotapi.NewInlineKeyboardButtonData(pendingBtn, "admin:orders:pending"),
		tgbotapi.NewInlineKeyboardButtonData(paidBtn, "admin:orders:paid"),
		tgbotapi.NewInlineKeyboardButtonData(deliveredBtn, "admin:orders:delivered"),
	)

	var rows [][]tgbotapi.InlineKeyboardButton
	rows = append(rows, filterRow)

	text := fmt.Sprintf("📦 <b>Orders Management</b> (Filter: %s - %d orders):\n\nTap an order to view details or update status:", filter, len(orders))
	if len(orders) == 0 {
		text += "\n\n<i>No orders found in this filter.</i>"
	} else {
		count := len(orders)
		if count > 15 {
			count = 15
		}
		for i := 0; i < count; i++ {
			o := orders[i]
			statusSymbol := "⏳"
			if o.Status == storage.OrderStatusPaid {
				statusSymbol = "💳"
			} else if o.Status == storage.OrderStatusDelivered {
				statusSymbol = "🚚"
			} else if o.Status == storage.OrderStatusCanceled {
				statusSymbol = "❌"
			}
			label := fmt.Sprintf("%s #%d | User %d | $%.2f", statusSymbol, o.ID, o.UserID, o.TotalUSD)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("admin:order:view:%d:%s", o.ID, filter)),
			))
		}
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ Admin Menu", "admin:menu"),
	))

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.send(edit)
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = kb
	b.send(msg)
}

func (b *Bot) sendAdminOrderView(chatID int64, msgID int, orderID int64, backFilter string, lang string) {
	ctx := context.Background()
	order, err := b.order.GetOrder(ctx, orderID)
	if err != nil {
		b.logger.Error("admin order view: get", "order_id", orderID, "error", err)
		return
	}

	statusDisplay := storage.StatusDisplay[order.Status]
	if statusDisplay == "" {
		statusDisplay = order.Status
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📦 <b>Order #%d</b>\n\n", order.ID))
	sb.WriteString(fmt.Sprintf("<b>Customer ID:</b> <code>%d</code>\n", order.UserID))
	sb.WriteString(fmt.Sprintf("<b>Status:</b> %s\n", statusDisplay))
	sb.WriteString(fmt.Sprintf("<b>Total:</b> $%.2f / %d ⭐\n", order.TotalUSD, order.TotalStars))
	if order.PaymentMethod != "" {
		sb.WriteString(fmt.Sprintf("<b>Payment:</b> %s\n", order.PaymentMethod))
	}
	if order.PromoCode != "" {
		sb.WriteString(fmt.Sprintf("<b>Promo:</b> %s (-%d%%)\n", order.PromoCode, order.DiscountPct))
	}
	sb.WriteString(fmt.Sprintf("<b>Created:</b> %s\n\n", order.CreatedAt.Format("02.01.2006 15:04")))

	if len(order.Items) > 0 {
		sb.WriteString("<b>Items:</b>\n")
		for _, item := range order.Items {
			sb.WriteString(fmt.Sprintf("• %s × %d — $%.2f\n", item.ProductName, item.Quantity, item.PriceUSD))
		}
	}

	var actionRows [][]tgbotapi.InlineKeyboardButton
	if order.Status == storage.OrderStatusPaid {
		actionRows = append(actionRows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Mark as Delivered", fmt.Sprintf("admin:order:deliver:%d:%s", order.ID, backFilter)),
		))
	}

	actionRows = append(actionRows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ Back to Orders", fmt.Sprintf("admin:orders:%s", backFilter)),
	))

	kb := tgbotapi.NewInlineKeyboardMarkup(actionRows...)
	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, sb.String())
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.send(edit)
		return
	}
	msg := tgbotapi.NewMessage(chatID, sb.String())
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = kb
	b.send(msg)
}

func (b *Bot) onAdminOrderDeliver(cbID string, chatID int64, msgID int, orderID int64, backFilter string, lang string) {
	ctx := context.Background()
	order, err := b.order.SetDelivered(ctx, orderID)
	if err != nil {
		b.logger.Error("admin set delivered", "order_id", orderID, "error", err)
		b.alert(cbID, b.t(lang, "admin_set_delivered_failed"))
		return
	}
	b.alert(cbID, fmt.Sprintf(b.t(lang, "admin_delivered_ok"), order.ID))

	b.sendReviewInvite(ctx, order)
	b.notifyAdmins(ctx, AdminEventOrderDelivered, fmt.Sprintf(b.t("en", "admin_order_delivered"), order.ID, order.UserID))
	b.outWebhook.Send(service.OutboundWebhookEvent{
		Event:      "order.delivered",
		OrderID:    order.ID,
		UserID:     order.UserID,
		TotalUSD:   order.TotalUSD,
		TotalStars: order.TotalStars,
	})

	b.sendAdminOrderView(chatID, msgID, orderID, backFilter, lang)
}

func (b *Bot) sendAdminPromos(chatID int64, msgID int, lang string) {
	ctx := context.Background()
	promos, err := b.promos.ListPromos(ctx)
	if err != nil {
		b.logger.Error("admin promos: list", "error", err)
		return
	}

	text := "🏷 <b>Promo Codes Management</b>\n\nActive promo codes:"
	var rows [][]tgbotapi.InlineKeyboardButton
	if len(promos) == 0 {
		text += "\n\n<i>No active promo codes.</i>"
	} else {
		for _, p := range promos {
			label := fmt.Sprintf("🏷 %s (-%d%%)", p.Code, p.Discount)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(label, "noop"),
				tgbotapi.NewInlineKeyboardButtonData("❌ Deactivate", fmt.Sprintf("admin:promo:del:%d", p.ID)),
			))
		}
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("➕ Add Promo Code", "admin:promo:add"),
	))
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("◀️ Admin Menu", "admin:menu"),
	))

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	if msgID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = "HTML"
		edit.ReplyMarkup = &kb
		b.send(edit)
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = kb
	b.send(msg)
}

func (b *Bot) onAdminPromoDelete(cbID string, chatID int64, msgID int, promoID int64, lang string) {
	ctx := context.Background()
	if err := b.promos.DeactivatePromo(ctx, promoID); err != nil {
		b.logger.Error("admin promo delete", "promo_id", promoID, "error", err)
		return
	}
	b.alert(cbID, b.t(lang, "admin_promo_deactivated"))
	b.sendAdminPromos(chatID, msgID, lang)
}

func (b *Bot) onAdminPromoAddPrompt(chatID, userID int64, lang string) {
	b.adminActions.Store(userID, "add_promo")
	msg := tgbotapi.NewMessage(chatID, "🏷 <b>Add Promo Code</b>\n\nPlease send the promo code and discount percentage separated by space (e.g., <code>SUMMER 15</code> for 15% discount).\n\nSend /cancel to abort.")
	msg.ParseMode = "HTML"
	b.send(msg)
}

func (b *Bot) onAdminExportOrders(chatID int64, lang string) {
	orders, err := b.order.GetAllOrders(context.Background(), "")
	if err != nil {
		b.logger.Error("export orders", "error", err)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_export_failed")))
		return
	}

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	_ = writer.Write([]string{"order_id", "user_id", "status", "total_usd", "total_stars", "payment_method", "promo_code", "created_at"})
	for _, o := range orders {
		_ = writer.Write([]string{
			strconv.FormatInt(o.ID, 10),
			strconv.FormatInt(o.UserID, 10),
			o.Status,
			fmt.Sprintf("%.2f", o.TotalUSD),
			strconv.Itoa(o.TotalStars),
			o.PaymentMethod,
			o.PromoCode,
			o.CreatedAt.Format(time.RFC3339),
		})
	}
	writer.Flush()

	doc := tgbotapi.NewDocument(chatID, tgbotapi.FileBytes{
		Name:  fmt.Sprintf("orders_%s.csv", time.Now().Format("2006-01-02")),
		Bytes: buf.Bytes(),
	})
	doc.Caption = fmt.Sprintf(b.t(lang, "admin_export_caption"), len(orders))
	b.send(doc)
}

func (b *Bot) handleAdminActionInput(msg *tgbotapi.Message, action string) bool {
	ctx := context.Background()
	lang := msg.From.LanguageCode
	chatID := msg.Chat.ID
	userID := msg.From.ID

	if msg.Text == "/cancel" {
		b.adminActions.Delete(userID)
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_cancelled")))
		b.sendAdminMenu(chatID, 0, lang)
		return true
	}

	switch action {
	case "add_category":
		b.adminActions.Delete(userID)
		parts := strings.Fields(msg.Text)
		if len(parts) < 1 {
			b.send(tgbotapi.NewMessage(chatID, "Invalid format. Send /cancel or try again with: 👟 Shoes"))
			return true
		}
		emoji := "📁"
		name := msg.Text
		if len(parts) >= 2 {
			emoji = parts[0]
			name = strings.Join(parts[1:], " ")
		}
		cat := &storage.Category{
			Name:     name,
			Emoji:    emoji,
			IsActive: true,
		}
		catID, err := b.products.CreateCategory(ctx, cat)
		if err != nil {
			b.logger.Error("admin action add category", "error", err)
			b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_category_create_failed")))
			return true
		}
		b.send(tgbotapi.NewMessage(chatID, fmt.Sprintf(b.t(lang, "admin_category_created"), catID, emoji, name)))
		b.sendAdminCategories(chatID, 0, lang)
		return true

	case "add_promo":
		b.adminActions.Delete(userID)
		parts := strings.Fields(msg.Text)
		if len(parts) < 2 {
			b.send(tgbotapi.NewMessage(chatID, "Invalid format. Please send: CODE DISCOUNT (e.g. SUMMER 15)"))
			return true
		}
		code := strings.ToUpper(parts[0])
		discount, err := strconv.Atoi(parts[1])
		if err != nil || discount <= 0 || discount > 100 {
			b.send(tgbotapi.NewMessage(chatID, "Discount must be a number between 1 and 100."))
			return true
		}
		promo := &storage.PromoCode{
			Code:     code,
			Discount: discount,
			IsActive: true,
		}
		_, err = b.promos.CreatePromo(ctx, promo)
		if err != nil {
			b.logger.Error("admin action add promo", "error", err)
			b.send(tgbotapi.NewMessage(chatID, "Failed to create promo code."))
			return true
		}
		b.send(tgbotapi.NewMessage(chatID, b.t(lang, "admin_promo_created")))
		b.sendAdminPromos(chatID, 0, lang)
		return true
	}
	return false
}
