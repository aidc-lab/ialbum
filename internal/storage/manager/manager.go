package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aidc-lab/ialbum/internal/auth"
	appdb "github.com/aidc-lab/ialbum/internal/db"
	storagecore "github.com/aidc-lab/ialbum/internal/storage"
	"github.com/aidc-lab/ialbum/internal/storage/baidu"
	localprovider "github.com/aidc-lab/ialbum/internal/storage/local"
	webdavprovider "github.com/aidc-lab/ialbum/internal/storage/webdav"
)

type Type string

const (
	Local  Type = "local"
	WebDAV Type = "webdav"
	Baidu  Type = "baidu"
)

type Connection struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Type          Type           `json:"type"`
	Config        map[string]any `json:"config"`
	Status        string         `json:"status"`
	StatusMessage string         `json:"statusMessage"`
	LastCheckedAt *time.Time     `json:"lastCheckedAt,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}
type CreateInput struct {
	Name    string
	Type    Type
	Config  map[string]any
	Secrets map[string]string
}
type UpdateInput struct {
	Name    *string
	Config  map[string]any
	Secrets map[string]string
}
type DeviceFlow struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	UserCode            string    `json:"userCode"`
	VerificationURL     string    `json:"verificationURL"`
	QRCodeURL           string    `json:"qrCodeURL,omitempty"`
	Status              string    `json:"status"`
	ErrorMessage        string    `json:"errorMessage,omitempty"`
	PollIntervalSeconds int64     `json:"pollIntervalSeconds"`
	ExpiresAt           time.Time `json:"expiresAt"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
	StorageID           string    `json:"storageId,omitempty"`
}
type secretEnvelope struct {
	Username, Password, AppKey, SecretKey, AccessToken, RefreshToken string
	ExpiresAt                                                        time.Time
}
type Manager struct {
	db         *appdb.DB
	sealer     *auth.Sealer
	httpClient *http.Client
}

func NewManager(db *appdb.DB, sealer *auth.Sealer) *Manager {
	return &Manager{db: db, sealer: sealer, httpClient: &http.Client{Timeout: 30 * time.Second}}
}

// ValidateSecrets makes a missing or wrong master key a startup error instead
// of leaving background jobs to discover undecryptable credentials later.
func (m *Manager) ValidateSecrets(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx, `SELECT id,type,secret_ciphertext,secret_nonce FROM storage_connections WHERE removed_at IS NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, typ string
		var ciphertext, nonce []byte
		if err := rows.Scan(&id, &typ, &ciphertext, &nonce); err != nil {
			return err
		}
		if _, err := m.sealer.Open(ciphertext, nonce, aad(id, Type(typ))); err != nil {
			return fmt.Errorf("decrypt storage connection %s: %w", id, err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	flowRows, err := m.db.QueryContext(ctx, `SELECT id,secret_ciphertext,secret_nonce,device_code_ciphertext,device_code_nonce FROM baidu_device_flows WHERE status='pending'`)
	if err != nil {
		return err
	}
	defer flowRows.Close()
	for flowRows.Next() {
		var id string
		var secretCipher, secretNonce, codeCipher, codeNonce []byte
		if err := flowRows.Scan(&id, &secretCipher, &secretNonce, &codeCipher, &codeNonce); err != nil {
			return err
		}
		if _, err := m.sealer.Open(secretCipher, secretNonce, "baidu-flow-secret:"+id); err != nil {
			return fmt.Errorf("decrypt Baidu device flow %s: %w", id, err)
		}
		if _, err := m.sealer.Open(codeCipher, codeNonce, "baidu-flow-code:"+id); err != nil {
			return fmt.Errorf("decrypt Baidu device code %s: %w", id, err)
		}
	}
	return flowRows.Err()
}
func (m *Manager) List(ctx context.Context) ([]Connection, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT id,name,type,config_json,status,status_message,last_checked_at,created_at,updated_at FROM storage_connections WHERE removed_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Connection, 0)
	for rows.Next() {
		var c Connection
		var typ, config string
		var checked sql.NullInt64
		var created, updated int64
		if err := rows.Scan(&c.ID, &c.Name, &typ, &config, &c.Status, &c.StatusMessage, &checked, &created, &updated); err != nil {
			return nil, err
		}
		c.Type = Type(typ)
		_ = json.Unmarshal([]byte(config), &c.Config)
		if checked.Valid {
			t := time.Unix(checked.Int64, 0).UTC()
			c.LastCheckedAt = &t
		}
		c.CreatedAt = time.Unix(created, 0).UTC()
		c.UpdatedAt = time.Unix(updated, 0).UTC()
		result = append(result, c)
	}
	return result, rows.Err()
}
func (m *Manager) Get(ctx context.Context, id string) (Connection, error) {
	var c Connection
	var typ, config string
	var checked sql.NullInt64
	var created, updated int64
	err := m.db.QueryRowContext(ctx, `SELECT id,name,type,config_json,status,status_message,last_checked_at,created_at,updated_at FROM storage_connections WHERE id=? AND removed_at IS NULL`, id).Scan(&c.ID, &c.Name, &typ, &config, &c.Status, &c.StatusMessage, &checked, &created, &updated)
	if err != nil {
		return c, err
	}
	c.Type = Type(typ)
	_ = json.Unmarshal([]byte(config), &c.Config)
	if checked.Valid {
		t := time.Unix(checked.Int64, 0).UTC()
		c.LastCheckedAt = &t
	}
	c.CreatedAt = time.Unix(created, 0).UTC()
	c.UpdatedAt = time.Unix(updated, 0).UTC()
	return c, nil
}
func (m *Manager) Create(ctx context.Context, input CreateInput) (Connection, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 80 {
		return Connection{}, errors.New("storage name is required and must not exceed 80 characters")
	}
	if input.Type != Local && input.Type != WebDAV && input.Type != Baidu {
		return Connection{}, errors.New("unsupported storage type")
	}
	id := newID()
	configJSON, err := json.Marshal(input.Config)
	if err != nil {
		return Connection{}, err
	}
	secrets := secretEnvelope{Username: input.Secrets["username"], Password: input.Secrets["password"], AppKey: input.Secrets["appKey"], SecretKey: input.Secrets["secretKey"], AccessToken: input.Secrets["accessToken"], RefreshToken: input.Secrets["refreshToken"]}
	if raw := input.Secrets["expiresAt"]; raw != "" {
		secrets.ExpiresAt, _ = time.Parse(time.RFC3339, raw)
	}
	secretJSON, _ := json.Marshal(secrets)
	ciphertext, nonce, err := m.sealer.Seal(secretJSON, aad(id, input.Type))
	if err != nil {
		return Connection{}, err
	}
	now := time.Now().UTC()
	_, err = m.db.ExecContext(ctx, `INSERT INTO storage_connections(id,name,type,config_json,secret_ciphertext,secret_nonce,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?, ?,?)`, id, input.Name, string(input.Type), string(configJSON), ciphertext, nonce, "unknown", now.Unix(), now.Unix())
	if err != nil {
		return Connection{}, err
	}
	provider, err := m.Provider(ctx, id)
	if err == nil {
		err = provider.Validate(ctx)
	}
	status, message := "ready", ""
	if err != nil {
		status = "error"
		message = err.Error()
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE storage_connections SET status=?,status_message=?,last_checked_at=?,updated_at=? WHERE id=?`, status, message, time.Now().Unix(), time.Now().Unix(), id)
	return m.Get(ctx, id)
}
func (m *Manager) Update(ctx context.Context, id string, input UpdateInput) (Connection, error) {
	current, err := m.Get(ctx, id)
	if err != nil {
		return Connection{}, err
	}
	name := current.Name
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
		if name == "" || len(name) > 80 {
			return Connection{}, errors.New("storage name is required and must not exceed 80 characters")
		}
	}
	config := current.Config
	if input.Config != nil {
		config = input.Config
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return Connection{}, err
	}
	var ciphertext, nonce []byte
	if err := m.db.QueryRowContext(ctx, `SELECT secret_ciphertext,secret_nonce FROM storage_connections WHERE id=? AND removed_at IS NULL`, id).Scan(&ciphertext, &nonce); err != nil {
		return Connection{}, err
	}
	plaintext, err := m.sealer.Open(ciphertext, nonce, aad(id, current.Type))
	if err != nil {
		return Connection{}, err
	}
	var secret secretEnvelope
	if err := json.Unmarshal(plaintext, &secret); err != nil {
		return Connection{}, err
	}
	if value, ok := input.Secrets["username"]; ok {
		secret.Username = value
	}
	if value, ok := input.Secrets["password"]; ok {
		secret.Password = value
	}
	if value, ok := input.Secrets["appKey"]; ok {
		secret.AppKey = value
	}
	if value, ok := input.Secrets["secretKey"]; ok {
		secret.SecretKey = value
	}
	raw, _ := json.Marshal(secret)
	ciphertext, nonce, err = m.sealer.Seal(raw, aad(id, current.Type))
	if err != nil {
		return Connection{}, err
	}
	now := time.Now().Unix()
	_, err = m.db.ExecContext(ctx, `UPDATE storage_connections SET name=?,config_json=?,secret_ciphertext=?,secret_nonce=?,status='unknown',status_message='',updated_at=? WHERE id=? AND removed_at IS NULL`, name, string(configJSON), ciphertext, nonce, now, id)
	if err != nil {
		return Connection{}, err
	}
	updated, testErr := m.Test(ctx, id)
	if testErr != nil {
		return updated, testErr
	}
	return updated, nil
}
func (m *Manager) Delete(ctx context.Context, id string) error {
	var count int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_storage_bindings b JOIN albums a ON a.id=b.album_id WHERE b.storage_id=? AND a.removed_at IS NULL`, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return storagecore.ErrConflict
	}
	result, err := m.db.ExecContext(ctx, `UPDATE storage_connections SET removed_at=?,updated_at=? WHERE id=? AND removed_at IS NULL`, time.Now().Unix(), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (m *Manager) Test(ctx context.Context, id string) (Connection, error) {
	provider, err := m.Provider(ctx, id)
	status, message := "ready", ""
	if err == nil {
		err = provider.Validate(ctx)
	}
	if err != nil {
		status = "error"
		message = err.Error()
	}
	_, updateErr := m.db.ExecContext(ctx, `UPDATE storage_connections SET status=?,status_message=?,last_checked_at=?,updated_at=? WHERE id=?`, status, message, time.Now().Unix(), time.Now().Unix(), id)
	if updateErr != nil {
		return Connection{}, updateErr
	}
	connection, getErr := m.Get(ctx, id)
	if err != nil {
		return connection, err
	}
	return connection, getErr
}
func (m *Manager) Provider(ctx context.Context, id string) (storagecore.Provider, error) {
	var typ, configJSON string
	var ciphertext, nonce []byte
	err := m.db.QueryRowContext(ctx, `SELECT type,config_json,secret_ciphertext,secret_nonce FROM storage_connections WHERE id=? AND removed_at IS NULL`, id).Scan(&typ, &configJSON, &ciphertext, &nonce)
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return nil, err
	}
	plaintext, err := m.sealer.Open(ciphertext, nonce, aad(id, Type(typ)))
	if err != nil {
		return nil, err
	}
	var secret secretEnvelope
	if err := json.Unmarshal(plaintext, &secret); err != nil {
		return nil, err
	}
	switch Type(typ) {
	case Local:
		return localprovider.New(stringValue(config, "root"))
	case WebDAV:
		return webdavprovider.New(stringValue(config, "url"), stringValue(config, "root"), secret.Username, secret.Password)
	case Baidu:
		return baidu.New(baidu.Config{AppKey: secret.AppKey, SecretKey: secret.SecretKey, Root: stringValue(config, "root"), Token: baidu.Token{AccessToken: secret.AccessToken, RefreshToken: secret.RefreshToken, ExpiresAt: secret.ExpiresAt}, SaveToken: func(ctx context.Context, t baidu.Token) error { return m.saveBaiduToken(ctx, id, config, secret, t) }}), nil
	default:
		return nil, storagecore.ErrUnsupported
	}
}
func (m *Manager) saveBaiduToken(ctx context.Context, id string, config map[string]any, secret secretEnvelope, token baidu.Token) error {
	secret.AccessToken = token.AccessToken
	secret.RefreshToken = token.RefreshToken
	secret.ExpiresAt = token.ExpiresAt
	raw, _ := json.Marshal(secret)
	ciphertext, nonce, err := m.sealer.Seal(raw, aad(id, Baidu))
	if err != nil {
		return err
	}
	_, err = m.db.ExecContext(ctx, `UPDATE storage_connections SET secret_ciphertext=?,secret_nonce=?,status='ready',status_message='',updated_at=? WHERE id=?`, ciphertext, nonce, time.Now().Unix(), id)
	return err
}
func (m *Manager) StartBaiduDeviceFlow(ctx context.Context, name, appKey, secretKey, root string) (DeviceFlow, error) {
	if strings.TrimSpace(name) == "" || appKey == "" || secretKey == "" {
		return DeviceFlow{}, errors.New("name, appKey and secretKey are required")
	}
	authorization, err := baidu.StartDeviceAuthorization(ctx, m.httpClient, "", appKey)
	if err != nil {
		return DeviceFlow{}, err
	}
	id := newID()
	configJSON, _ := json.Marshal(map[string]any{"appKey": appKey, "root": root})
	secretCipher, secretNonce, err := m.sealer.Seal([]byte(secretKey), "baidu-flow-secret:"+id)
	if err != nil {
		return DeviceFlow{}, err
	}
	codeCipher, codeNonce, err := m.sealer.Seal([]byte(authorization.DeviceCode), "baidu-flow-code:"+id)
	if err != nil {
		return DeviceFlow{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(time.Duration(authorization.ExpiresIn) * time.Second)
	_, err = m.db.ExecContext(ctx, `INSERT INTO baidu_device_flows(id,name,config_json,secret_ciphertext,secret_nonce,device_code_ciphertext,device_code_nonce,user_code,verification_url,qr_url,poll_interval_seconds,status,expires_at,next_poll_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, name, string(configJSON), secretCipher, secretNonce, codeCipher, codeNonce, authorization.UserCode, authorization.VerificationURL, authorization.QRCodeURL, authorization.Interval, "pending", expires.Unix(), now.Add(time.Duration(authorization.Interval)*time.Second).Unix(), now.Unix(), now.Unix())
	if err != nil {
		return DeviceFlow{}, err
	}
	return m.GetBaiduDeviceFlow(ctx, id)
}
func (m *Manager) GetBaiduDeviceFlow(ctx context.Context, id string) (DeviceFlow, error) {
	flow, config, secretCipher, secretNonce, codeCipher, codeNonce, nextPoll, err := m.loadFlow(ctx, id)
	if err != nil {
		return flow, err
	}
	now := time.Now().UTC()
	if flow.Status != "pending" || now.Before(nextPoll) {
		return flow, nil
	}
	if now.After(flow.ExpiresAt) {
		_, _ = m.db.ExecContext(ctx, `UPDATE baidu_device_flows SET status='expired',updated_at=? WHERE id=?`, now.Unix(), id)
		return m.GetBaiduDeviceFlow(ctx, id)
	}
	secret, err := m.sealer.Open(secretCipher, secretNonce, "baidu-flow-secret:"+id)
	if err != nil {
		return flow, err
	}
	deviceCode, err := m.sealer.Open(codeCipher, codeNonce, "baidu-flow-code:"+id)
	if err != nil {
		return flow, err
	}
	appKey := stringValue(config, "appKey")
	token, state, pollErr := baidu.PollDeviceAuthorization(ctx, m.httpClient, "", appKey, string(secret), string(deviceCode))
	switch state {
	case "authorization_pending", "pending":
		_, _ = m.db.ExecContext(ctx, `UPDATE baidu_device_flows SET next_poll_at=?,updated_at=? WHERE id=?`, now.Add(time.Duration(flow.PollIntervalSeconds)*time.Second).Unix(), now.Unix(), id)
	case "slow_down":
		flow.PollIntervalSeconds += 5
		_, _ = m.db.ExecContext(ctx, `UPDATE baidu_device_flows SET poll_interval_seconds=?,next_poll_at=?,updated_at=? WHERE id=?`, flow.PollIntervalSeconds, now.Add(time.Duration(flow.PollIntervalSeconds)*time.Second).Unix(), now.Unix(), id)
	case "authorization_declined", "access_denied":
		_, _ = m.db.ExecContext(ctx, `UPDATE baidu_device_flows SET status='declined',error_message=?,updated_at=? WHERE id=?`, errorText(pollErr), now.Unix(), id)
	case "expired_token":
		_, _ = m.db.ExecContext(ctx, `UPDATE baidu_device_flows SET status='expired',error_message=?,updated_at=? WHERE id=?`, errorText(pollErr), now.Unix(), id)
	default:
		if pollErr != nil {
			_, _ = m.db.ExecContext(ctx, `UPDATE baidu_device_flows SET status='error',error_message=?,updated_at=? WHERE id=?`, pollErr.Error(), now.Unix(), id)
		} else {
			connection, createErr := m.Create(ctx, CreateInput{Name: flow.Name, Type: Baidu, Config: map[string]any{"root": stringValue(config, "root")}, Secrets: map[string]string{"appKey": appKey, "secretKey": string(secret), "accessToken": token.AccessToken, "refreshToken": token.RefreshToken, "expiresAt": token.ExpiresAt.Format(time.RFC3339)}})
			if createErr != nil {
				_, _ = m.db.ExecContext(ctx, `UPDATE baidu_device_flows SET status='error',error_message=?,updated_at=? WHERE id=?`, createErr.Error(), now.Unix(), id)
			} else {
				_, _ = m.db.ExecContext(ctx, `UPDATE baidu_device_flows SET status='authorized',error_message=?,updated_at=? WHERE id=?`, connection.ID, now.Unix(), id)
			}
		}
	}
	return m.GetBaiduDeviceFlow(ctx, id)
}
func (m *Manager) CancelBaiduDeviceFlow(ctx context.Context, id string) error {
	result, err := m.db.ExecContext(ctx, `UPDATE baidu_device_flows SET status='cancelled',updated_at=? WHERE id=? AND status='pending'`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (m *Manager) loadFlow(ctx context.Context, id string) (DeviceFlow, map[string]any, []byte, []byte, []byte, []byte, time.Time, error) {
	var f DeviceFlow
	var configJSON string
	var secretCipher, secretNonce, codeCipher, codeNonce []byte
	var expires, nextPoll, created, updated int64
	err := m.db.QueryRowContext(ctx, `SELECT id,name,config_json,secret_ciphertext,secret_nonce,device_code_ciphertext,device_code_nonce,user_code,verification_url,qr_url,poll_interval_seconds,status,error_message,expires_at,next_poll_at,created_at,updated_at FROM baidu_device_flows WHERE id=?`, id).Scan(&f.ID, &f.Name, &configJSON, &secretCipher, &secretNonce, &codeCipher, &codeNonce, &f.UserCode, &f.VerificationURL, &f.QRCodeURL, &f.PollIntervalSeconds, &f.Status, &f.ErrorMessage, &expires, &nextPoll, &created, &updated)
	var config map[string]any
	_ = json.Unmarshal([]byte(configJSON), &config)
	f.ExpiresAt = time.Unix(expires, 0).UTC()
	f.CreatedAt = time.Unix(created, 0).UTC()
	f.UpdatedAt = time.Unix(updated, 0).UTC()
	if f.Status == "authorized" {
		f.StorageID = f.ErrorMessage
		f.ErrorMessage = ""
	}
	return f, config, secretCipher, secretNonce, codeCipher, codeNonce, time.Unix(nextPoll, 0).UTC(), err
}
func (m *Manager) GetQRCode(ctx context.Context, id string) (string, error) {
	flow, err := m.GetBaiduDeviceFlow(ctx, id)
	if err != nil {
		return "", err
	}
	if flow.QRCodeURL != "" {
		return flow.QRCodeURL, nil
	}
	u, err := url.Parse(flow.VerificationURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("user_code", flow.UserCode)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
func aad(id string, typ Type) string { return "storage:" + id + ":" + string(typ) + ":v1" }
func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}
func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
