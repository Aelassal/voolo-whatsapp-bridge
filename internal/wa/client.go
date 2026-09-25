// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Package wa connects the protocol to whatsmeow: it runs pairing, maps
// whatsmeow events to protocol events, streams history under the ack window
// and carries out the client's commands. Everything that touches the network
// goes through the Client interface, so tests drive the bridge with a fake
// and never reach WhatsApp.
package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/transport"
)

// AccountInfo is what the store knows about the linked account.
type AccountInfo struct {
	JID          types.JID // phone-number JID without device, empty when unpaired
	LID          types.JID
	PushName     string
	BusinessName string
}

// Client is the part of whatsmeow the bridge uses. *whatsmeow.Client plus the
// store accessors of realClient implement it; tests use a fake.
type Client interface {
	AddEventHandler(handler whatsmeow.EventHandler) uint32
	Connect() error
	Disconnect()
	IsConnected() bool
	IsLoggedIn() bool
	PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType whatsmeow.PairClientType, clientDisplayName string) (string, error)
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	MarkRead(ctx context.Context, ids []types.MessageID, timestamp time.Time, chat, sender types.JID, receiptTypeExtra ...types.ReceiptType) error
	DownloadMediaWithPath(ctx context.Context, directPath string, encFileHash, fileHash, mediaKey []byte, mediaType whatsmeow.MediaType, mmsType string, allowNoHash bool) ([]byte, error)
	Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	Logout(ctx context.Context) error
	ParseWebMessage(chatJID types.JID, webMsg *waWeb.WebMessageInfo) (*events.Message, error)
	DownloadHistorySync(ctx context.Context, notif *waE2E.HistorySyncNotification, synchronousStorage bool) (*waHistorySync.HistorySync, error)
	DeleteMedia(ctx context.Context, appInfo whatsmeow.MediaType, directPath string, encFileHash []byte, encHandle string) error
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
	GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
	// SendAppState writes one app-state patch (set_pin, revision 2).
	SendAppState(ctx context.Context, patch appstate.PatchInfo) error
	// GetProfilePictureInfo and FetchPicture serve fetch_avatar (revision 2).
	GetProfilePictureInfo(ctx context.Context, jid types.JID, params *whatsmeow.GetProfilePictureParams) (*types.ProfilePictureInfo, error)
	// FetchPicture downloads a profile picture URL through the guarded media
	// client, at most max bytes.
	FetchPicture(ctx context.Context, url string, max int64) ([]byte, error)

	Account() AccountInfo
	PNForLID(ctx context.Context, lid types.JID) types.JID
	Contact(ctx context.Context, jid types.JID) types.ContactInfo
}

type realClient struct {
	*whatsmeow.Client
	media *http.Client // the guarded media client (allowlist + body limit)
}

// FetchPicture downloads a profile picture over the guarded media client: the
// host allowlist of §10.2 and a body limit of max bytes apply.
func (c realClient) FetchPicture(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(transport.WithBodyLimit(ctx, max), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "https" {
		return nil, errors.New("picture url is not https")
	}
	resp, err := c.media.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("picture download: status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (c realClient) Account() AccountInfo {
	d := c.Store
	if d == nil || d.ID == nil {
		return AccountInfo{}
	}
	return AccountInfo{JID: d.ID.ToNonAD(), LID: d.GetLID().ToNonAD(), PushName: d.PushName, BusinessName: d.BusinessName}
}

func (c realClient) PNForLID(ctx context.Context, lid types.JID) types.JID {
	if c.Store == nil || c.Store.LIDs == nil {
		return types.EmptyJID
	}
	pn, err := c.Store.LIDs.GetPNForLID(ctx, lid)
	if err != nil {
		return types.EmptyJID
	}
	return pn.ToNonAD()
}

func (c realClient) Contact(ctx context.Context, jid types.JID) types.ContactInfo {
	if c.Store == nil || c.Store.Contacts == nil {
		return types.ContactInfo{}
	}
	ci, err := c.Store.Contacts.GetContact(ctx, jid)
	if err != nil {
		return types.ContactInfo{}
	}
	return ci
}

// NewRealClient builds a whatsmeow client on a device store. Every HTTP client
// (pre-login and logged-in websocket, media) goes through the allowlisted
// transport; the media client also applies the download body limit
// (transport.NewMediaClient).
func NewRealClient(device *store.Device, httpClient, mediaClient *http.Client, log waLog.Logger) Client {
	cli := whatsmeow.NewClient(device, log)
	cli.SetPreLoginHTTPClient(httpClient)
	cli.SetWebsocketHTTPClient(httpClient)
	cli.SetMediaHTTPClient(mediaClient)
	cli.ManualHistorySyncDownload = true // the bridge downloads at the pace of the ack window
	cli.EnableAutoReconnect = true
	cli.InitialAutoReconnect = true
	cli.EmitAppStateEventsOnFullSync = false
	return realClient{cli, mediaClient}
}

// ConfigureDevice sets the linked-device properties before pairing: the name
// shown for QR links and the history window asked of the phone (S5).
func ConfigureDevice(deviceName string, historyDays int) {
	store.SetOSInfo(deviceName, [3]uint32{0, 1, 0})
	cfg := store.DeviceProps.HistorySyncConfig
	cfg.FullSyncDaysLimit = proto.Uint32(uint32(historyDays))
	cfg.RecentSyncDaysLimit = proto.Uint32(uint32(historyDays))
	store.DeviceProps.RequireFullSync = proto.Bool(false)
}

// RefreshWAVersion fetches the current WhatsApp Web version through the
// allowlisted client and applies it (used once after a 405).
func RefreshWAVersion(ctx context.Context, httpClient *http.Client) error {
	v, err := whatsmeow.GetLatestVersion(ctx, httpClient)
	if err != nil {
		return err
	}
	store.SetWAVersion(*v)
	return nil
}
