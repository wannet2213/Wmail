package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"wmail/internal/db"
	"wmail/internal/models"
)

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func ok(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]bool{"success": true}) }

func (a *API) AdminOverview(w http.ResponseWriter, r *http.Request, u *models.User) {
	users, err := a.db.ListUsers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	mailboxes, _ := a.db.ListMailboxes()
	grants, _ := a.db.ListGrants()
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "mailboxes": mailboxes, "grants": grants})
}

func (a *API) AdminCreateUser(w http.ResponseWriter, r *http.Request, u *models.User) {
	var req struct {
		Username, Password string
		IsAdmin            bool
	}
	if !decode(w, r, &req) {
		return
	}
	if _, err := a.db.CreateUser(req.Username, req.Password, req.IsAdmin); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			http.Error(w, "Username sudah dipakai.", http.StatusConflict)
			return
		}
		writeDBError(w, err)
		return
	}
	ok(w)
}

func (a *API) AdminUpdateUser(w http.ResponseWriter, r *http.Request, u *models.User) {
	id, valid := pathID(w, r)
	if !valid {
		return
	}
	var req struct {
		Password *string
		IsAdmin  *bool
	}
	if !decode(w, r, &req) {
		return
	}
	if req.IsAdmin != nil {
		if err := a.db.SetAdmin(id, *req.IsAdmin); err != nil {
			writeDBError(w, err)
			return
		}
	}
	if req.Password != nil {
		if err := a.db.SetPassword(id, *req.Password); err != nil {
			writeDBError(w, err)
			return
		}
	}
	ok(w)
}

func (a *API) AdminDeleteUser(w http.ResponseWriter, r *http.Request, u *models.User) {
	id, valid := pathID(w, r)
	if !valid {
		return
	}
	if id == u.ID {
		http.Error(w, "Tidak bisa menghapus akun sendiri.", http.StatusBadRequest)
		return
	}
	if err := a.db.DeleteUser(id); err != nil {
		writeDBError(w, err)
		return
	}
	ok(w)
}

func (a *API) AdminCreateMailbox(w http.ResponseWriter, r *http.Request, u *models.User) {
	var req struct{ Address, DisplayName string }
	if !decode(w, r, &req) {
		return
	}
	if _, err := a.db.CreateMailbox(req.Address, req.DisplayName); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			http.Error(w, "Alamat sudah ada.", http.StatusConflict)
			return
		}
		writeDBError(w, err)
		return
	}
	ok(w)
}

func (a *API) AdminUpdateMailbox(w http.ResponseWriter, r *http.Request, u *models.User) {
	id, valid := pathID(w, r)
	if !valid {
		return
	}
	var req struct{ DisplayName string }
	if !decode(w, r, &req) {
		return
	}
	if err := a.db.UpdateMailboxName(id, req.DisplayName); err != nil {
		writeDBError(w, err)
		return
	}
	ok(w)
}

func (a *API) AdminDeleteMailbox(w http.ResponseWriter, r *http.Request, u *models.User) {
	id, valid := pathID(w, r)
	if !valid {
		return
	}
	if err := a.db.DeleteMailbox(id); err != nil {
		writeDBError(w, err)
		return
	}
	ok(w)
}

// AdminSetGrant sets a user's access to a mailbox: "none", "receive" or "send".
func (a *API) AdminSetGrant(w http.ResponseWriter, r *http.Request, u *models.User) {
	var req struct {
		UserID, MailboxID int64
		Access            string
	}
	if !decode(w, r, &req) {
		return
	}
	var err error
	switch req.Access {
	case "none":
		err = a.db.DeleteGrant(req.UserID, req.MailboxID)
	case "receive", "send":
		err = a.db.SetGrant(req.UserID, req.MailboxID, req.Access == "send")
	default:
		http.Error(w, "access harus none, receive, atau send", http.StatusBadRequest)
		return
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	ok(w)
}

// AdminGetSettings returns every known key, secrets masked.
func (a *API) AdminGetSettings(w http.ResponseWriter, r *http.Request, u *models.User) {
	s, err := a.db.GetSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]string{}
	for _, k := range db.SettingKeys {
		v := s[k.Key]
		if k.Secret {
			v = db.MaskSecret(v)
		}
		out[k.Key] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// AdminSaveSettings updates web-editable keys; values still carrying the mask are left unchanged.
func (a *API) AdminSaveSettings(w http.ResponseWriter, r *http.Request, u *models.User) {
	var req map[string]string
	if !decode(w, r, &req) {
		return
	}
	updates := map[string]string{}
	for key, v := range req {
		k, known := db.LookupSettingKey(key)
		if !known || !k.Web {
			http.Error(w, "setting tidak bisa diubah dari web: "+key, http.StatusBadRequest)
			return
		}
		v = strings.TrimSpace(v)
		if k.Secret && strings.Contains(v, db.MaskMarker) {
			continue
		}
		if err := ValidateSetting(key, v); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		updates[key] = v
	}
	for key, v := range updates {
		if err := a.db.SetSetting(key, v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	ok(w)
}

// ValidateSetting rejects values the mailer would misread; shared with the CLI.
func ValidateSetting(key, v string) error {
	switch key {
	case "active_provider":
		if v != "" && v != "auto" && v != "resend" && v != "ses" {
			return errors.New("active_provider harus auto, resend, atau ses")
		}
	case "resend_daily_limit", "resend_monthly_limit":
		if n, err := strconv.Atoi(v); v != "" && (err != nil || n < 0) {
			return errors.New(key + " harus angka >= 0")
		}
	}
	return nil
}
