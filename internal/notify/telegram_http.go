package notify

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
)

func (h *Handler) handleTelegram(w http.ResponseWriter, r *http.Request) {
	h.writeTelegram(w, r)
}

// writeTelegram answers with the space's bot and the member's chat.
func (h *Handler) writeTelegram(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	out := apitypes.TelegramStatus{Available: h.tg != nil, CanManage: p.Role == family.RoleOwner, Bot: nullable.NewNullNullable[apitypes.TelegramBot]()}
	bot, err := h.store.Bot(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if bot != nil {
		out.Bot = nullable.NewNullableWithValue(apitypes.TelegramBot{Username: bot.Username, TokenLast4: bot.TokenLast4})
		chat, err := h.store.ChatOf(r.Context(), p.UserID)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		out.Linked = chat != nil && chat.SpaceID == p.SpaceID
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleSetBot(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	if h.tg == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "telegram is off: the server has no encryption key to keep the bot's token")
		return
	}
	var req apitypes.TelegramBotInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	token := strings.TrimSpace(req.Token)
	// @BotFather's tokens are «<digits>:<35 characters>»; anything with a
	// slash or a space would change the address it is put in.
	if len(token) < 20 || len(token) > 100 || strings.ContainsAny(token, "/?# ") || !strings.Contains(token, ":") {
		httpjson.Error(w, http.StatusBadRequest, "token must be the bot's token from @BotFather")
		return
	}
	name, err := h.tg.API.GetMe(r.Context(), token)
	switch {
	case errors.Is(err, ErrBadToken):
		httpjson.Error(w, http.StatusBadRequest, "telegram does not know this token")
		return
	case err != nil:
		h.log.Warn("telegram getMe", "error", err)
		httpjson.Error(w, http.StatusBadGateway, "telegram did not answer; try again later")
		return
	}
	if err := h.store.SetBot(r.Context(), Bot{SpaceID: p.SpaceID, TokenCiphertext: h.tg.Box.Seal([]byte(token)), TokenLast4: token[len(token)-4:], Username: name}); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeTelegram(w, r)
}

func (h *Handler) handleDeleteBot(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	found, err := h.store.DeleteBot(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if !found {
		httpjson.Error(w, http.StatusNotFound, "the space has no bot")
		return
	}
	h.writeTelegram(w, r)
}

func (h *Handler) handleLink(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	bot, err := h.store.Bot(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if bot == nil || h.tg == nil {
		httpjson.Error(w, http.StatusConflict, "the space has no bot yet: the owner gives it one first")
		return
	}
	code, expires, err := h.store.NewLinkCode(r.Context(), p.SpaceID, p.UserID, time.Now())
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, apitypes.TelegramLink{Code: code, Url: "https://t.me/" + bot.Username + "?start=" + code, ExpiresAt: expires})
}

func (h *Handler) handleUnlink(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	found, err := h.store.Unlink(r.Context(), p.UserID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if !found {
		httpjson.Error(w, http.StatusNotFound, "no chat is tied")
		return
	}
	h.writeTelegram(w, r)
}
