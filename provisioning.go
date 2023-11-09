package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"maunium.net/go/mautrix/id"
)

type provisioningHandle struct {
	context context.Context
	cancel  context.CancelFunc
	channel <-chan signalmeow.ProvisioningResponse
}

type ProvisioningAPI struct {
	bridge              *SignalBridge
	log                 zerolog.Logger
	provisioningHandles []provisioningHandle
	provisioningUsers   map[string]int
}

func (prov *ProvisioningAPI) Init() {
	prov.log.Debug().Msgf("Enabling provisioning API at %v", prov.bridge.Config.Bridge.Provisioning.Prefix)
	prov.provisioningUsers = make(map[string]int)
	r := prov.bridge.AS.Router.PathPrefix(prov.bridge.Config.Bridge.Provisioning.Prefix).Subrouter()
	r.Use(prov.AuthMiddleware)
	r.HandleFunc("/v2/link/new", prov.LinkNew).Methods(http.MethodPost)
	r.HandleFunc("/v2/link/wait/scan", prov.LinkWaitForScan).Methods(http.MethodPost)
	r.HandleFunc("/v2/link/wait/account", prov.LinkWaitForAccount).Methods(http.MethodPost)
	r.HandleFunc("/v2/logout", prov.Logout).Methods(http.MethodPost)
	r.HandleFunc("/v2/resolve_identifier/{phonenum}", prov.ResolveIdentifier).Methods(http.MethodGet)
	r.HandleFunc("/v2/pm/{phonenum}", prov.StartPM).Methods(http.MethodPost)
}

type responseWrap struct {
	http.ResponseWriter
	statusCode int
}

func jsonResponse(w http.ResponseWriter, status int, response interface{}) {
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

var _ http.Hijacker = (*responseWrap)(nil)

func (rw *responseWrap) WriteHeader(statusCode int) {
	rw.ResponseWriter.WriteHeader(statusCode)
	rw.statusCode = statusCode
}

func (rw *responseWrap) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response does not implement http.Hijacker")
	}
	return hijacker.Hijack()
}

func (prov *ProvisioningAPI) AuthMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			auth = auth[len("Bearer "):]
		}
		if auth != prov.bridge.Config.Bridge.Provisioning.SharedSecret {
			prov.log.Info().Msg("Authentication token does not match shared secret")
			jsonResponse(w, http.StatusForbidden, map[string]interface{}{
				"error":   "Authentication token does not match shared secret",
				"errcode": "M_FORBIDDEN",
			})
			return
		}
		userID := r.URL.Query().Get("user_id")
		user := prov.bridge.GetUserByMXID(id.UserID(userID))
		start := time.Now()
		wWrap := &responseWrap{w, 200}
		h.ServeHTTP(wWrap, r.WithContext(context.WithValue(r.Context(), "user", user)))
		duration := time.Now().Sub(start).Seconds()
		prov.log.Info().Msgf("%s %s from %s took %.2f seconds and returned status %d", r.Method, r.URL.Path, user.MXID, duration, wWrap.statusCode)
	})
}

type Error struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	ErrCode string `json:"errcode"`
}

type Response struct {
	Success bool   `json:"success"`
	Status  string `json:"status"`

	// For response in LinkNew
	SessionID string `json:"session_id,omitempty"`
	URI       string `json:"uri,omitempty"`

	// For response in LinkWaitForAccount
	UUID   string `json:"uuid,omitempty"`
	Number string `json:"number,omitempty"`

	// For response in ResolveIdentifier
	ResolveIdentifierResponse
}

type ResolveIdentifierResponse struct {
	RoomID      string                             `json:"room_id"`
	ChatID      ResolveIdentifierResponseChatID    `json:"chat_id"`
	JustCreated bool                               `json:"just_created"`
	OtherUser   ResolveIdentifierResponseOtherUser `json:"other_user"`
}

type ResolveIdentifierResponseChatID struct {
	UUID   string `json:"uuid"`
	Number string `json:"number"`
}

type ResolveIdentifierResponseOtherUser struct {
	MXID        string `json:"mxid"`
	DisplayName string `json:"displayname"`
	AvatarURL   string `json:"avatar_url"`
}

func (prov *ProvisioningAPI) resolveIdentifier(user *User, phoneNum string) (int, *ResolveIdentifierResponse, error) {
	if !strings.HasPrefix(phoneNum, "+") {
		phoneNum = "+" + phoneNum
	}
	contact, err := user.SignalDevice.ContactByE164(phoneNum)
	if err != nil {
		prov.log.Err(err).Msgf("ResolveIdentifier from %v, error looking up contact", user.MXID)
		return http.StatusInternalServerError, nil, fmt.Errorf("Error looking up number in local contact list: %w", err)
	}
	if contact == nil {
		prov.log.Debug().Msgf("ResolveIdentifier from %v, contact not found", user.MXID)
		return http.StatusNotFound, nil, fmt.Errorf("The bridge does not have the Signal ID for the number %s", phoneNum)
	}

	portal := user.GetPortalByChatID(contact.UUID)
	puppet := prov.bridge.GetPuppetBySignalID(contact.UUID)

	return http.StatusOK, &ResolveIdentifierResponse{
		RoomID: portal.MXID.String(),
		ChatID: ResolveIdentifierResponseChatID{
			UUID:   contact.UUID,
			Number: phoneNum,
		},
		OtherUser: ResolveIdentifierResponseOtherUser{
			MXID:        puppet.MXID.String(),
			DisplayName: puppet.Name,
			AvatarURL:   puppet.AvatarURL.String(),
		},
	}, nil
}

func (prov *ProvisioningAPI) ResolveIdentifier(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	phoneNum, _ := mux.Vars(r)["phonenum"]
	prov.log.Debug().Msgf("ResolveIdentifier from %v, phone number: %v", user.MXID, phoneNum)

	status, resp, err := prov.resolveIdentifier(user, phoneNum)
	if err != nil {
		errCode := "M_INTERNAL"
		if status == http.StatusNotFound {
			prov.log.Debug().Msgf("ResolveIdentifier from %v, contact not found", user.MXID)
			errCode = "M_NOT_FOUND"
		} else {
			prov.log.Err(err).Msgf("ResolveIdentifier from %v, error looking up contact", user.MXID)
		}
		jsonResponse(w, status, Error{
			Success: false,
			Error:   err.Error(),
			ErrCode: errCode,
		})
		return
	}
	jsonResponse(w, status, Response{
		Success:                   true,
		Status:                    "ok",
		ResolveIdentifierResponse: *resp,
	})
}

func (prov *ProvisioningAPI) StartPM(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	phoneNum, _ := mux.Vars(r)["phonenum"]
	prov.log.Debug().Msgf("StartPM from %v, phone number: %v", user.MXID, phoneNum)

	status, resp, err := prov.resolveIdentifier(user, phoneNum)
	if err != nil {
		errCode := "M_INTERNAL"
		if status == http.StatusNotFound {
			prov.log.Debug().Msgf("StartPM from %v, contact not found", user.MXID)
			errCode = "M_NOT_FOUND"
		} else {
			prov.log.Err(err).Msgf("StartPM from %v, error looking up contact", user.MXID)
		}
		jsonResponse(w, status, Error{
			Success: false,
			Error:   err.Error(),
			ErrCode: errCode,
		})
		return
	}

	justCreated := false
	portal := user.GetPortalByChatID(resp.ChatID.UUID)
	if portal.MXID == "" {
		justCreated = true
		if err := portal.CreateMatrixRoom(user, nil); err != nil {
			prov.log.Err(err).Msgf("StartPM from %v, error creating Matrix room", user.MXID)
			jsonResponse(w, http.StatusInternalServerError, Error{
				Success: false,
				Error:   "Error creating Matrix room",
				ErrCode: "M_INTERNAL",
			})
			return
		}
	}
	resp.JustCreated = justCreated
	if justCreated {
		status = http.StatusCreated
	}

	jsonResponse(w, status, Response{
		Success:                   true,
		Status:                    "ok",
		ResolveIdentifierResponse: *resp,
	})
}

func (prov *ProvisioningAPI) LinkNew(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	prov.log.Debug().Msgf("LinkNew from %v", user.MXID)
	if existingSessionID, ok := prov.provisioningUsers[user.MXID.String()]; ok {
		prov.log.Warn().Msgf("LinkNew from %v, user already has a pending provisioning request (%d), cancelling", user.MXID, existingSessionID)
		prov.CancelLink(user)
	}

	provChan, err := user.Login()
	if err != nil {
		prov.log.Err(err).Msg("Error logging in")
		jsonResponse(w, http.StatusInternalServerError, Error{
			Success: false,
			Error:   "Error logging in",
			ErrCode: "M_INTERNAL",
		})
		return
	}
	provisioningCtx, cancel := context.WithCancel(context.Background())
	handle := provisioningHandle{
		context: provisioningCtx,
		cancel:  cancel,
		channel: provChan,
	}
	prov.provisioningHandles = append(prov.provisioningHandles, handle)
	sessionID := len(prov.provisioningHandles) - 1
	prov.provisioningUsers[user.MXID.String()] = sessionID
	prov.log.Debug().Msgf("LinkNew from %v, waiting for provisioning response", user.MXID)

	select {
	case resp := <-provChan:
		if resp.Err != nil || resp.State == signalmeow.StateProvisioningError {
			prov.log.Err(resp.Err).Msg("Error getting provisioning URL")
			jsonResponse(w, http.StatusInternalServerError, Error{
				Success: false,
				Error:   resp.Err.Error(),
				ErrCode: "M_INTERNAL",
			})
			return
		}
		if resp.State != signalmeow.StateProvisioningURLReceived {
			prov.log.Err(err).Msgf("LinkNew from %v, unexpected state: %v", user.MXID, resp.State)
			jsonResponse(w, http.StatusInternalServerError, Error{
				Success: false,
				Error:   "Unexpected state",
				ErrCode: "M_INTERNAL",
			})
			return
		}

		prov.log.Debug().Msgf("LinkNew from %v, provisioning URL received", user.MXID)
		jsonResponse(w, http.StatusOK, Response{
			Success:   true,
			Status:    "provisioning_url_received",
			SessionID: fmt.Sprintf("%v", sessionID),
			URI:       resp.ProvisioningUrl,
		})
		return
	case <-time.After(30 * time.Second):
		prov.log.Warn().Msg("Timeout waiting for provisioning response (new)")
		jsonResponse(w, http.StatusGatewayTimeout, Error{
			Success: false,
			Error:   "Timeout waiting for provisioning response (new)",
			ErrCode: "M_TIMEOUT",
		})
		return
	}
}

func (prov *ProvisioningAPI) LinkWaitForScan(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	body := struct {
		SessionID string `json:"session_id"`
	}{}
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		prov.log.Err(err).Msg("Error decoding JSON body")
		jsonResponse(w, http.StatusBadRequest, Error{
			Success: false,
			Error:   "Error decoding JSON body",
			ErrCode: "M_BAD_JSON",
		})
		return
	}

	sessionID, err := strconv.Atoi(body.SessionID)
	if err != nil {
		prov.log.Err(err).Msg("Error decoding JSON body")
		jsonResponse(w, http.StatusBadRequest, Error{
			Success: false,
			Error:   "Error decoding JSON body",
			ErrCode: "M_BAD_JSON",
		})
		return
	}
	prov.log.Debug().Msgf("LinkWaitForScan from %v, session_id: %v", user.MXID, sessionID)
	if userSessionID, ok := prov.provisioningUsers[user.MXID.String()]; ok && userSessionID != sessionID {
		prov.log.Warn().Msgf("LinkWaitForAccount from %v, session_id %v does not match user's session_id %v", user.MXID, sessionID, userSessionID)
		jsonResponse(w, http.StatusBadRequest, Error{
			Success: false,
			Error:   "session_id does not match user's session_id",
			ErrCode: "M_BAD_JSON",
		})
		return
	}
	handle := prov.provisioningHandles[sessionID]

	select {
	case resp := <-handle.channel:
		if resp.Err != nil || resp.State == signalmeow.StateProvisioningError {
			prov.log.Err(resp.Err).Msg("Error waiting for scan")
			// If context was cancelled be chill
			if errors.Is(resp.Err, context.Canceled) {
				prov.log.Debug().Msg("Context cancelled waiting for scan")
				return
			}
			// If we error waiting for the scan, treat it as a normal error not 5xx
			// so that the client will retry quietly. Also, it's really not an internal
			// error, sitting with a WS open waiting for a scan is inherently flaky.
			jsonResponse(w, http.StatusBadRequest, Error{
				Success: false,
				Error:   resp.Err.Error(),
				ErrCode: "M_INTERNAL",
			})
			return
		}
		if resp.State != signalmeow.StateProvisioningDataReceived {
			prov.log.Err(err).Msgf("LinkWaitForScan from %v, unexpected state: %v", user.MXID, resp.State)
			jsonResponse(w, http.StatusInternalServerError, Error{
				Success: false,
				Error:   "Unexpected state",
				ErrCode: "M_INTERNAL",
			})
			return
		}
		prov.log.Debug().Msgf("LinkWaitForScan from %v, provisioning data received", user.MXID)
		jsonResponse(w, http.StatusOK, Response{
			Success: true,
			Status:  "provisioning_data_received",
		})

		// Update user with SignalID
		if resp.ProvisioningData.AciUuid != "" {
			user.SignalID = resp.ProvisioningData.AciUuid
			user.SignalUsername = resp.ProvisioningData.Number
			user.Update()
		}
		return
	case <-time.After(45 * time.Second):
		prov.log.Warn().Msg("Timeout waiting for provisioning response (scan)")
		// Using 400 here to match the old bridge
		jsonResponse(w, http.StatusBadRequest, Error{
			Success: false,
			Error:   "Timeout waiting for QR code scan",
			ErrCode: "M_BAD_REQUEST",
		})
		return
	}
}

func (prov *ProvisioningAPI) LinkWaitForAccount(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	body := struct {
		SessionID  string `json:"session_id"`
		DeviceName string `json:"device_name"`
	}{}
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		prov.log.Err(err).Msg("Error decoding JSON body")
		jsonResponse(w, http.StatusBadRequest, Error{
			Success: false,
			Error:   "Error decoding JSON body",
			ErrCode: "M_BAD_JSON",
		})
		return
	}
	sessionID, err := strconv.Atoi(body.SessionID)
	if err != nil {
		prov.log.Err(err).Msg("Error decoding JSON body")
		jsonResponse(w, http.StatusBadRequest, Error{
			Success: false,
			Error:   "Error decoding JSON body",
			ErrCode: "M_BAD_JSON",
		})
		return
	}
	deviceName := body.DeviceName
	prov.log.Debug().Msgf("LinkWaitForAccount from %v, session_id: %v, device_name: %v", user.MXID, sessionID, deviceName)
	if userSessionID, ok := prov.provisioningUsers[user.MXID.String()]; ok && userSessionID != sessionID {
		prov.log.Warn().Msgf("LinkWaitForAccount from %v, session_id %v does not match user's session_id %v", user.MXID, sessionID, userSessionID)
		jsonResponse(w, http.StatusBadRequest, Error{
			Success: false,
			Error:   "session_id does not match user's session_id",
			ErrCode: "M_BAD_JSON",
		})
		return
	}
	handle := prov.provisioningHandles[sessionID]

	select {
	case resp := <-handle.channel:
		if resp.Err != nil || resp.State == signalmeow.StateProvisioningError {
			prov.log.Err(resp.Err).Msg("Error waiting for account")
			jsonResponse(w, http.StatusInternalServerError, Error{
				Success: false,
				Error:   resp.Err.Error(),
				ErrCode: "M_INTERNAL",
			})
			return
		}
		if resp.State != signalmeow.StateProvisioningPreKeysRegistered {
			prov.log.Err(err).Msgf("LinkWaitForAccount from %v, unexpected state: %v", user.MXID, resp.State)
			jsonResponse(w, http.StatusInternalServerError, Error{
				Success: false,
				Error:   "Unexpected state",
				ErrCode: "M_INTERNAL",
			})
			return
		}

		prov.log.Debug().Msgf("LinkWaitForAccount from %v, prekeys registered", user.MXID)
		jsonResponse(w, http.StatusOK, Response{
			Success: true,
			Status:  "prekeys_registered",
			UUID:    user.SignalID,
			Number:  user.SignalUsername,
		})

		// Connect to Signal!!
		user.Connect()
		return
	case <-time.After(30 * time.Second):
		prov.log.Warn().Msg("Timeout waiting for provisioning response (account)")
		jsonResponse(w, http.StatusGatewayTimeout, Error{
			Success: false,
			Error:   "Timeout waiting for provisioning response (account)",
			ErrCode: "M_TIMEOUT",
		})
		return
	}
}

func (prov *ProvisioningAPI) CancelLink(user *User) {
	if sessionID, ok := prov.provisioningUsers[user.MXID.String()]; ok {
		prov.log.Debug().Msgf("CancelLink called for %v, clearing session %v", user.MXID, sessionID)
		if sessionID >= len(prov.provisioningHandles) {
			prov.log.Warn().Msgf("CancelLink called for %v, session %v does not exist", user.MXID, sessionID)
			return
		}
		if prov.provisioningHandles[sessionID].cancel != nil {
			prov.provisioningHandles[sessionID].cancel()
		}
		prov.provisioningHandles[sessionID] = provisioningHandle{}
		delete(prov.provisioningUsers, user.MXID.String())
	} else {
		prov.log.Debug().Msgf("CancelLink called for %v, no session found", user.MXID)
	}
}

func (prov *ProvisioningAPI) Logout(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	prov.log.Debug().Msgf("Logout called from %v (but not logging out)", user.MXID)
	prov.CancelLink(user)

	// For now do nothing - we need this API to return 200 to be compatible with
	// the old Signal bridge, which needed a call to Logout before allowing LinkNew
	// to be called, but we don't actually want to logout, we want to allow a reconnect.
	jsonResponse(w, http.StatusOK, Response{
		Success: true,
		Status:  "logged_out",
	})
}
