// mautrix-signal - A Matrix-signal puppeting bridge.
// Copyright (C) 2023 Scott Weber
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exerrors"
	"go.mau.fi/util/exfmt"
	"go.mau.fi/util/ffmpeg"
	"go.mau.fi/util/jsontime"
	"go.mau.fi/util/variationselector"
	cwebp "go.mau.fi/webp"
	"golang.org/x/exp/slices"
	"golang.org/x/image/webp"
	"google.golang.org/protobuf/proto"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-signal/database"
	"go.mau.fi/mautrix-signal/msgconv/matrixfmt"
	"go.mau.fi/mautrix-signal/msgconv/signalfmt"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	signalpb "go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf"
)

type portalSignalMessage struct {
	message signalmeow.IncomingSignalMessage
	user    *User
	sender  *Puppet
	sync    bool
}

type portalMatrixMessage struct {
	evt  *event.Event
	user *User
}

type Portal struct {
	*database.Portal

	bridge *SignalBridge
	log    zerolog.Logger

	roomCreateLock sync.Mutex
	encryptLock    sync.Mutex

	signalMessages chan portalSignalMessage
	matrixMessages chan portalMatrixMessage

	currentlyTyping     []id.UserID
	currentlyTypingLock sync.Mutex

	latestReadTimestamp uint64 // Cache the latest read timestamp to avoid unnecessary read receipts

	relayUser *User
}

const recentMessageBufferSize = 32

func init() {
	event.TypeMap[event.StateBridge] = reflect.TypeOf(CustomBridgeInfoContent{})
	event.TypeMap[event.StateHalfShotBridge] = reflect.TypeOf(CustomBridgeInfoContent{})
}

// ** Interfaces that Portal implements **

var _ bridge.Portal = (*Portal)(nil)

var _ bridge.ReadReceiptHandlingPortal = (*Portal)(nil)
var _ bridge.TypingPortal = (*Portal)(nil)
var _ bridge.DisappearingPortal = (*Portal)(nil)

//var _ bridge.MembershipHandlingPortal = (*Portal)(nil)
//var _ bridge.MetaHandlingPortal = (*Portal)(nil)

// ** bridge.Portal Interface **

func (portal *Portal) IsEncrypted() bool {
	return portal.Encrypted
}

func (portal *Portal) MarkEncrypted() {
	portal.Encrypted = true
	err := portal.Update(context.TODO())
	if err != nil {
		portal.log.Err(err).Msg("Failed to update portal in database after marking as encrypted")
	}
}

func (portal *Portal) ReceiveMatrixEvent(user bridge.User, evt *event.Event) {
	if user.GetPermissionLevel() >= bridgeconfig.PermissionLevelUser || portal.HasRelaybot() {
		portal.matrixMessages <- portalMatrixMessage{user: user.(*User), evt: evt}
	}
}

func (portal *Portal) GetRelayUser() *User {
	if !portal.HasRelaybot() {
		return nil
	} else if portal.relayUser == nil {
		portal.relayUser = portal.bridge.GetUserByMXID(portal.RelayUserID)
	}
	return portal.relayUser
}

func isUUID(s string) bool {
	if _, uuidErr := uuid.Parse(s); uuidErr == nil {
		return true
	}
	return false
}

func (portal *Portal) IsPrivateChat() bool {
	// If ChatID is a UUID, it's a private chat, otherwise it's base64 and a group chat
	return isUUID(portal.ChatID)
}

func (portal *Portal) MainIntent() *appservice.IntentAPI {
	if portal.IsPrivateChat() {
		return portal.bridge.GetPuppetBySignalID(portal.UserID()).DefaultIntent()
	}

	return portal.bridge.Bot
}

type CustomBridgeInfoContent struct {
	event.BridgeEventContent
	RoomType string `json:"com.beeper.room_type,omitempty"`
}

func (portal *Portal) getBridgeInfo() (string, CustomBridgeInfoContent) {
	bridgeInfo := event.BridgeEventContent{
		BridgeBot: portal.bridge.Bot.UserID,
		Creator:   portal.MainIntent().UserID,
		Protocol: event.BridgeInfoSection{
			ID:          "signal",
			DisplayName: "Signal",
			AvatarURL:   portal.bridge.Config.AppService.Bot.ParsedAvatar.CUString(),
			ExternalURL: "https://signal.org/",
		},
		Channel: event.BridgeInfoSection{
			ID:          portal.ChatID,
			DisplayName: portal.Name,
			AvatarURL:   portal.AvatarURL.CUString(),
		},
	}
	var bridgeInfoStateKey string
	bridgeInfoStateKey = fmt.Sprintf("fi.mau.signal://signal/%s", portal.ChatID)
	bridgeInfo.Channel.ExternalURL = fmt.Sprintf("https://signal.me/#p/%s", portal.ChatID)
	var roomType string
	if portal.IsPrivateChat() {
		roomType = "dm"
	}
	return bridgeInfoStateKey, CustomBridgeInfoContent{bridgeInfo, roomType}
}

func (portal *Portal) UpdateBridgeInfo() {
	if len(portal.MXID) == 0 {
		portal.log.Debug().Msg("Not updating bridge info: no Matrix room created")
		return
	}
	portal.log.Debug().Msg("Updating bridge info...")
	stateKey, content := portal.getBridgeInfo()
	_, err := portal.MainIntent().SendStateEvent(portal.MXID, event.StateBridge, stateKey, content)
	if err != nil {
		portal.log.Warn().Msgf("Failed to update m.bridge: %v", err)
	}
	// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
	_, err = portal.MainIntent().SendStateEvent(portal.MXID, event.StateHalfShotBridge, stateKey, content)
	if err != nil {
		portal.log.Warn().Msgf("Failed to update uk.half-shot.bridge: %v", err)
	}
}

// ** bridge.ChildOverride methods (for SignalBridge in main.go) **

func (br *SignalBridge) GetAllPortalsWithMXID() []*Portal {
	portals, err := br.dbPortalsToPortals(br.DB.Portal.GetAllWithMXID(context.TODO()))
	if err != nil {
		br.ZLog.Err(err).Msg("Failed to get all portals with mxid")
		return nil
	}
	return portals
}

func (br *SignalBridge) GetAllIPortals() (iportals []bridge.Portal) {
	portals, err := br.dbPortalsToPortals(br.DB.Portal.GetAllWithMXID(context.TODO()))
	if err != nil {
		br.ZLog.Err(err).Msg("Failed to get all portals with mxid")
		return nil
	}
	iportals = make([]bridge.Portal, len(portals))
	for i, portal := range portals {
		iportals[i] = portal
	}
	return iportals
}

func (br *SignalBridge) dbPortalsToPortals(dbPortals []*database.Portal, err error) ([]*Portal, error) {
	if err != nil {
		return nil, err
	}
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()

	output := make([]*Portal, len(dbPortals))
	for index, dbPortal := range dbPortals {
		if dbPortal == nil {
			continue
		}

		portal, ok := br.portalsByID[dbPortal.PortalKey]
		if !ok {
			portal = br.loadPortal(context.TODO(), dbPortal, nil)
		}

		output[index] = portal
	}

	return output, nil
}

// ** Portal Creation and Message Handling **

func (br *SignalBridge) NewPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: br,
		log:    br.ZLog.With().Str("chat_id", dbPortal.ChatID).Logger(),

		signalMessages: make(chan portalSignalMessage, br.Config.Bridge.PortalMessageBuffer),
		matrixMessages: make(chan portalMatrixMessage, br.Config.Bridge.PortalMessageBuffer),
	}

	go portal.messageLoop()

	return portal
}

func (portal *Portal) messageLoop() {
	for {
		select {
		case msg := <-portal.matrixMessages:
			portal.handleMatrixMessages(msg)
		case msg := <-portal.signalMessages:
			portal.handleSignalMessages(msg)
		}
	}
}

func (portal *Portal) handleMatrixMessages(msg portalMatrixMessage) {
	// If we have no SignalDevice, the bridge isn't logged in properly,
	// so send BAD_CREDENTIALS so the user knows
	if !msg.user.SignalDevice.IsDeviceLoggedIn() && !portal.HasRelaybot() {
		go portal.sendMessageMetrics(msg.evt, errUserNotLoggedIn, "Ignoring", nil)
		msg.user.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Message: "You have been logged out of Signal, please reconnect"})
		return
	}
	log := portal.log.With().Str("event_id", msg.evt.ID.String()).Logger()
	ctx := log.WithContext(context.TODO())

	switch msg.evt.Type {
	case event.EventMessage, event.EventSticker:
		portal.handleMatrixMessage(ctx, msg.user, msg.evt)
	case event.EventRedaction:
		portal.handleMatrixRedaction(ctx, msg.user, msg.evt)
	case event.EventReaction:
		portal.handleMatrixReaction(ctx, msg.user, msg.evt)
	default:
		log.Warn().Str("type", msg.evt.Type.String()).Msg("Unhandled matrix message type")
	}
}

func (portal *Portal) handleMatrixMessage(ctx context.Context, sender *User, evt *event.Event) {
	evtTS := time.UnixMilli(evt.Timestamp)
	timings := messageTimings{
		initReceive:  evt.Mautrix.ReceivedAt.Sub(evtTS),
		decrypt:      evt.Mautrix.DecryptionDuration,
		totalReceive: time.Since(evtTS),
	}
	implicitRRStart := time.Now()
	timings.implicitRR = time.Since(implicitRRStart)
	start := time.Now()

	messageAge := timings.totalReceive
	ms := metricSender{portal: portal, timings: &timings}
	portal.log.Debug().Msgf("Received message %s from %s (age: %s)", evt.ID, evt.Sender, messageAge)

	errorAfter := portal.bridge.Config.Bridge.MessageHandlingTimeout.ErrorAfter
	deadline := portal.bridge.Config.Bridge.MessageHandlingTimeout.Deadline
	isScheduled, _ := evt.Content.Raw["com.beeper.scheduled"].(bool)
	if isScheduled {
		portal.log.Debug().Msgf("%s is a scheduled message, extending handling timeouts", evt.ID)
		errorAfter *= 10
		deadline *= 10
	}

	if errorAfter > 0 {
		remainingTime := errorAfter - messageAge
		if remainingTime < 0 {
			go ms.sendMessageMetrics(evt, errTimeoutBeforeHandling, "Timeout handling", true)
			return
		} else if remainingTime < 1*time.Second {
			portal.log.Warn().Msgf("Message %s was delayed before reaching the bridge, only have %s (of %s timeout) until delay warning", evt.ID, remainingTime, errorAfter)
		}
		go func() {
			time.Sleep(remainingTime)
			ms.sendMessageMetrics(evt, errMessageTakingLong, "Timeout handling", false)
		}()
	}

	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	timings.preproc = time.Since(start)
	start = time.Now()

	msg, err := portal.convertMatrixMessage(ctx, sender, evt)
	if err != nil || msg == nil {
		if err == nil {
			err = fmt.Errorf("msg is nil")
		}
		portal.log.Error().Msgf("Error converting message %s: %v", evt.ID, err)
		go ms.sendMessageMetrics(evt, err, "Error converting", true)
		return
	}

	timestamp := *msg.DataMessage.Timestamp
	if timestamp == 0 {
		timestamp = uint64(start.UnixMilli())
	}
	timings.convert = time.Since(start)
	start = time.Now()

	// If the portal has disappearing messages enabled, set the expiration time
	if portal.ExpirationTime > 0 {
		signalmeow.AddExpiryToDataMessage(msg, uint32(portal.ExpirationTime))
	}
	if !sender.IsLoggedIn() {
		sender = portal.GetRelayUser()
	}
	err = portal.sendSignalMessage(ctx, msg, sender, evt.ID)

	timings.totalSend = time.Since(start)
	go ms.sendMessageMetrics(evt, err, "Error sending", true)
	if err == nil {
		portal.storeMessageInDB(ctx, evt.ID, sender.SignalID, timestamp, 0)
		if portal.ExpirationTime > 0 {
			portal.addDisappearingMessage(ctx, evt.ID, int64(portal.ExpirationTime), true)
		}
	}
}

func (portal *Portal) handleMatrixRedaction(ctx context.Context, sender *User, evt *event.Event) {
	log := zerolog.Ctx(ctx)
	// Find the original signal message based on eventID
	dbMessage, err := portal.bridge.DB.Message.GetByMXID(ctx, evt.Redacts)
	if err != nil {
		log.Err(err).Msg("Failed to get redaction target message")
	}
	// Might be a reaction redaction, find the original message for the reaction
	dbReaction, err := portal.bridge.DB.Reaction.GetByMXID(ctx, evt.Redacts)
	if err != nil {
		log.Err(err).Msg("Failed to get redaction target reaction")
	}
	if dbMessage == nil && dbReaction == nil {
		portal.sendMessageStatusCheckpointFailed(evt, errors.New("could not find original message or reaction"))
		log.Warn().Msg("No target message or reaction found for redaction")
		return
	}

	if !sender.IsLoggedIn() {
		sender = portal.GetRelayUser()
	}

	// If this is a message redaction, send a redaction to Signal
	if dbMessage != nil {
		msg := signalmeow.DataMessageForDelete(dbMessage.Timestamp)
		err = portal.sendSignalMessage(ctx, msg, sender, evt.ID)
		if err != nil {
			portal.sendMessageStatusCheckpointFailed(evt, err)
			log.Err(err).Msg("Failed to send message redaction to Signal")
			return
		}
		err = dbMessage.Delete(ctx)
		if err != nil {
			log.Err(err).Msg("Failed to delete redacted message from database")
		} else if otherParts, err := portal.bridge.DB.Message.GetAllPartsBySignalID(ctx, dbMessage.Sender, dbMessage.Timestamp, portal.Receiver); err != nil {
			log.Err(err).Msg("Failed to get other parts of redacted message from database")
		} else if len(otherParts) > 0 {
			// If there are other parts of the message, send a redaction for each of them
			for _, otherPart := range otherParts {
				_, err = portal.MainIntent().RedactEvent(portal.MXID, otherPart.MXID, mautrix.ReqRedact{
					Reason: "Other part of Signal message redacted",
					TxnID:  "mxsg_partredact_" + otherPart.MXID.String(),
				})
				if err != nil {
					log.Err(err).
						Str("part_event_id", otherPart.MXID.String()).
						Int("part_index", otherPart.PartIndex).
						Msg("Failed to redact other part of redacted message")
				}
				err = otherPart.Delete(ctx)
				if err != nil {
					log.Err(err).
						Str("part_event_id", otherPart.MXID.String()).
						Int("part_index", otherPart.PartIndex).
						Msg("Failed to delete other part of redacted message from database")
				}
			}
		}

	}

	if dbReaction != nil {
		msg := signalmeow.DataMessageForReaction(dbReaction.Emoji, dbReaction.MsgAuthor, dbReaction.MsgTimestamp, true)
		err = portal.sendSignalMessage(ctx, msg, sender, evt.ID)
		if err != nil {
			portal.sendMessageStatusCheckpointFailed(evt, err)
			log.Err(err).Msg("Failed to send reaction redaction to Signal")
			return
		}
		err = dbReaction.Delete(ctx)
		if err != nil {
			log.Err(err).Msg("Failed to delete redacted reaction from database")
		}
	}

	portal.sendMessageStatusCheckpointSuccess(evt)
}

func (portal *Portal) handleMatrixReaction(ctx context.Context, sender *User, evt *event.Event) {
	log := zerolog.Ctx(ctx)
	if !sender.IsLoggedIn() {
		log.Error().Msg("Cannot relay reaction from non-logged-in user. Ignoring")
		return
	}
	// Find the original signal message based on eventID
	relatedEventID := evt.Content.AsReaction().RelatesTo.EventID
	dbMessage, err := portal.bridge.DB.Message.GetByMXID(ctx, relatedEventID)
	if err != nil {
		portal.sendMessageStatusCheckpointFailed(evt, err)
		log.Err(err).Msg("Failed to get reaction target message")
		return
	} else if dbMessage == nil {
		portal.sendMessageStatusCheckpointFailed(evt, errors.New("could not find original message for reaction"))
		log.Warn().Msg("No target message found for reaction")
		return
	}
	emoji := evt.Content.AsReaction().RelatesTo.Key
	signalEmoji := variationselector.FullyQualify(emoji) // Signal seems to require fully qualified emojis
	targetAuthorUUID := dbMessage.Sender
	targetTimestamp := dbMessage.Timestamp
	msg := signalmeow.DataMessageForReaction(signalEmoji, targetAuthorUUID, targetTimestamp, false)
	err = portal.sendSignalMessage(context.Background(), msg, sender, evt.ID)
	if err != nil {
		portal.sendMessageStatusCheckpointFailed(evt, err)
		portal.log.Error().Msgf("Failed to send reaction %s", evt.ID)
		return
	}

	// Signal only allows one reaction from each user
	// Check if there's an existing reaction in the database for this sender and redact/delete it
	dbReaction, err := portal.bridge.DB.Reaction.GetBySignalID(
		ctx,
		targetAuthorUUID,
		targetTimestamp,
		sender.SignalID,
		portal.Receiver,
	)
	if err != nil {
		log.Err(err).Msg("Failed to get existing reaction from database")
	} else if dbReaction != nil {
		log.Debug().Str("existing_event_id", dbReaction.MXID.String()).Msg("Redacting existing reaction after sending new one")
		_, err = portal.MainIntent().RedactEvent(portal.MXID, dbReaction.MXID)
		if err != nil {
			log.Err(err).Msg("Failed to redact existing reaction")
		}
		// TODO update instead of deleting
		err = dbReaction.Delete(ctx)
		if err != nil {
			log.Err(err).Msg("Failed to delete reaction from database")
		}
	}

	// Store our new reaction in the database
	portal.storeReactionInDB(ctx, evt.ID, sender.SignalID, targetAuthorUUID, targetTimestamp, signalEmoji)

	portal.sendMessageStatusCheckpointSuccess(evt)
}

func (portal *Portal) downloadAndDecryptMatrixMedia(ctx context.Context, content *event.MessageEventContent) ([]byte, error) {
	var file *event.EncryptedFileInfo
	rawMXC := content.URL
	if content.File != nil {
		file = content.File
		rawMXC = file.URL
	}
	mxc, err := rawMXC.Parse()
	if err != nil {
		return nil, err
	}
	data, err := portal.MainIntent().DownloadBytesContext(ctx, mxc)
	if err != nil {
		return nil, exerrors.NewDualError(errMediaDownloadFailed, err)
	}
	if file != nil {
		err = file.DecryptInPlace(data)
		if err != nil {
			return nil, exerrors.NewDualError(errMediaDecryptFailed, err)
		}
	}
	return data, nil
}

func convertWebPtoPNG(webpImage []byte) ([]byte, error) {
	webpDecoded, err := webp.Decode(bytes.NewReader(webpImage))
	if err != nil {
		return nil, fmt.Errorf("failed to decode webp image: %w", err)
	}

	var pngBuffer bytes.Buffer
	if err = png.Encode(&pngBuffer, webpDecoded); err != nil {
		return nil, fmt.Errorf("failed to encode png image: %w", err)
	}

	return pngBuffer.Bytes(), nil
}

type PaddedImage struct {
	image.Image
	Size    int
	OffsetX int
	OffsetY int
}

func (img *PaddedImage) Bounds() image.Rectangle {
	return image.Rect(0, 0, img.Size, img.Size)
}

func (img *PaddedImage) At(x, y int) color.Color {
	return img.Image.At(x+img.OffsetX, y+img.OffsetY)
}

func convertToWebPSticker(img []byte) ([]byte, error) {
	decodedImg, _, err := image.Decode(bytes.NewReader(img))
	if err != nil {
		return img, fmt.Errorf("failed to decode image: %w", err)
	}

	bounds := decodedImg.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width != height {
		paddedImg := &PaddedImage{
			Image:   decodedImg,
			OffsetX: bounds.Min.Y,
			OffsetY: bounds.Min.X,
		}
		if width > height {
			paddedImg.Size = width
			paddedImg.OffsetY -= (paddedImg.Size - height) / 2
		} else {
			paddedImg.Size = height
			paddedImg.OffsetX -= (paddedImg.Size - width) / 2
		}
		decodedImg = paddedImg
	}

	var webpBuffer bytes.Buffer
	if err = cwebp.Encode(&webpBuffer, decodedImg, nil); err != nil {
		return img, fmt.Errorf("failed to encode webp image: %w", err)
	}

	return webpBuffer.Bytes(), nil
}

func convertImage(ctx context.Context, mimeType string, image []byte) (string, []byte, error) {
	var outMimeType string
	var outImage []byte
	var err error
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif":
		// Allowed
		outMimeType = mimeType
		outImage = image
	case "image/webp":
		outMimeType = "image/png"
		outImage, err = convertWebPtoPNG(image)
	default:
		return "", nil, fmt.Errorf("%w %q", errMediaUnsupportedType, mimeType)
	}
	if err != nil {
		return "", nil, fmt.Errorf("%w (%s to %s)", errMediaConvertFailed, mimeType, outMimeType)
	}
	return outMimeType, outImage, nil
}

func convertSticker(ctx context.Context, mimeType string, sticker []byte, width, height int) (string, []byte, error) {
	var outMimeType string = mimeType
	var outSticker []byte = sticker
	var err error
	if mimeType != "image/webp" || width != height {
		outSticker, err = convertToWebPSticker(sticker)
		outMimeType = "image/webp"
	}
	if err != nil {
		return "", nil, fmt.Errorf("%w (%s to %s)", errMediaConvertFailed, mimeType, outMimeType)
	}
	return outMimeType, outSticker, nil
}

func convertVideo(ctx context.Context, mimeType string, video []byte) (string, []byte, error) {
	var outMimeType string
	var outVideo []byte
	var err error
	switch mimeType {
	case "video/mp4", "video/3gpp":
		// Allowed
		outMimeType = mimeType
		outVideo = video
	case "video/webm":
		outMimeType = "video/mp4"
		outVideo, err = ffmpeg.ConvertBytes(ctx, video, ".mp4", []string{"-f", "webm"}, []string{
			"-pix_fmt", "yuv420p", "-c:v", "libx264",
		}, mimeType)
	default:
		return "", nil, fmt.Errorf("%w %q in video message", errMediaUnsupportedType, mimeType)
	}
	if err != nil {
		return "", nil, fmt.Errorf("%w (%s to %s)", errMediaConvertFailed, mimeType, outMimeType)
	}
	return outMimeType, outVideo, nil
}

func (portal *Portal) convertMatrixMessage(ctx context.Context, sender *User, evt *event.Event) (*signalmeow.SignalContent, error) {
	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		return nil, fmt.Errorf("%w %T", errUnexpectedParsedContentType, evt.Content.Parsed)
	}

	if evt.Type == event.EventSticker {
		content.MsgType = event.MessageType(event.EventSticker.Type)
	}
	realSenderMXID := sender.MXID
	isRelay := false
	if !sender.IsLoggedIn() {
		if !portal.HasRelaybot() {
			return nil, errUserNotLoggedIn
		}
		sender = portal.GetRelayUser()
		if !sender.IsLoggedIn() {
			return nil, errRelaybotNotLoggedIn
		}
		isRelay = true
	}
	var outgoingMessage *signalmeow.SignalContent
	relaybotFormatted := isRelay && portal.addRelaybotFormat(realSenderMXID, content)
	if relaybotFormatted && content.FileName == "" {
		content.FileName = content.Body
	}

	if evt.Type == event.EventSticker {
		if relaybotFormatted {
			// Stickers can't have captions, so force relaybot stickers to be images
			content.MsgType = event.MsgImage
		} else {
			content.MsgType = event.MessageType(event.EventSticker.Type)
		}
	}

	switch content.MsgType {
	case event.MsgText, event.MsgEmote, event.MsgNotice:
		if content.MsgType == event.MsgNotice && !portal.bridge.Config.Bridge.BridgeNotices {
			return nil, errMNoticeDisabled
		}
		if content.MsgType == event.MsgEmote && !relaybotFormatted {
			content.Body = "/me " + content.Body
			if content.FormattedBody != "" {
				content.FormattedBody = "/me " + content.FormattedBody
			}
		}
		outgoingMessage = signalmeow.DataMessageForText(matrixfmt.Parse(matrixFormatParams, content))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

	case event.MsgImage:
		fileName := content.Body
		var caption string
		var ranges []*signalpb.BodyRange
		if content.FileName != "" && (content.Body != content.FileName || content.Format == event.FormatHTML) {
			fileName = content.FileName
			caption, ranges = matrixfmt.Parse(matrixFormatParams, content)
		}
		image, err := portal.downloadAndDecryptMatrixMedia(ctx, content)
		if err != nil {
			return nil, err
		}
		newMimeType, convertedImage, err := convertImage(ctx, content.GetInfo().MimeType, image)
		if err != nil {
			return nil, err
		}
		attachmentPointer, err := signalmeow.UploadAttachment(sender.SignalDevice, convertedImage, newMimeType, fileName)
		if err != nil {
			return nil, err
		}
		attachmentPointer.Height = proto.Uint32(uint32(content.GetInfo().Height))
		attachmentPointer.Width = proto.Uint32(uint32(content.GetInfo().Width))
		outgoingMessage = signalmeow.DataMessageForAttachment(attachmentPointer, caption, ranges)

	case event.MessageType(event.EventSticker.Type):
		var emoji *string
		// TODO check for single grapheme cluster?
		if len([]rune(content.Body)) == 1 {
			emoji = proto.String(variationselector.Remove(content.Body))
		}
		image, err := portal.downloadAndDecryptMatrixMedia(ctx, content)
		if err != nil {
			return nil, err
		}
		newMimeType, convertedSticker, err := convertSticker(ctx, content.GetInfo().MimeType, image, content.GetInfo().Width, content.GetInfo().Height)
		if err != nil {
			return nil, err
		}
		attachmentPointer, err := signalmeow.UploadAttachment(sender.SignalDevice, convertedSticker, newMimeType, content.FileName)
		if err != nil {
			return nil, err
		}
		attachmentPointer.Height = proto.Uint32(uint32(content.GetInfo().Height))
		attachmentPointer.Width = proto.Uint32(uint32(content.GetInfo().Width))
		attachmentPointer.Flags = proto.Uint32(uint32(signalpb.AttachmentPointer_BORDERLESS))
		outgoingMessage = &signalmeow.SignalContent{
			DataMessage: &signalpb.DataMessage{
				Timestamp: proto.Uint64(uint64(time.Now().UnixMilli())),
				Sticker: &signalpb.DataMessage_Sticker{
					// Signal iOS validates that pack id/key are of the correct length.
					// Android is fine with any non-nil values (like a zero-length byte string).
					PackId:    make([]byte, 16),
					PackKey:   make([]byte, 32),
					StickerId: proto.Uint32(0),

					Data:  (*signalpb.AttachmentPointer)(attachmentPointer),
					Emoji: emoji,
				},
			},
		}
	case event.MsgVideo:
		fileName := content.Body
		var caption string
		var ranges []*signalpb.BodyRange
		if content.FileName != "" && (content.Body != content.FileName || content.Format == event.FormatHTML) {
			fileName = content.FileName
			caption, ranges = matrixfmt.Parse(matrixFormatParams, content)
		}
		image, err := portal.downloadAndDecryptMatrixMedia(ctx, content)
		if err != nil {
			return nil, err
		}
		newMimeType, convertedVideo, err := convertVideo(ctx, content.GetInfo().MimeType, image)
		if err != nil {
			return nil, err
		}
		attachmentPointer, err := signalmeow.UploadAttachment(sender.SignalDevice, convertedVideo, newMimeType, fileName)
		if err != nil {
			return nil, err
		}
		outgoingMessage = signalmeow.DataMessageForAttachment(attachmentPointer, caption, ranges)

	case event.MsgAudio:
		fileName := content.Body
		var caption string
		var ranges []*signalpb.BodyRange
		if content.FileName != "" && (content.Body != content.FileName || content.Format == event.FormatHTML) {
			fileName = content.FileName
			caption, ranges = matrixfmt.Parse(matrixFormatParams, content)
		}
		data, err := portal.downloadAndDecryptMatrixMedia(ctx, content)
		if err != nil {
			return nil, err
		}
		_, isVoice := evt.Content.Raw["org.matrix.msc3245.voice"]
		mime := content.GetInfo().MimeType
		if isVoice {
			data, err = ffmpeg.ConvertBytes(ctx, data, ".m4a", []string{}, []string{"-c:a", "aac"}, mime)
			if err != nil {
				return nil, err
			}
			mime = "audio/aac"
			fileName += ".m4a"
		}
		attachmentPointer, err := signalmeow.UploadAttachment(sender.SignalDevice, data, mime, fileName)
		if err != nil {
			return nil, err
		}
		if isVoice {
			attachmentPointer.Flags = proto.Uint32(uint32(signalpb.AttachmentPointer_VOICE_MESSAGE))
		}
		outgoingMessage = signalmeow.DataMessageForAttachment(attachmentPointer, caption, ranges)

	case event.MsgFile:
		fileName := content.Body
		var caption string
		var ranges []*signalpb.BodyRange
		if content.FileName != "" && (content.Body != content.FileName || content.Format == event.FormatHTML) {
			fileName = content.FileName
			caption, ranges = matrixfmt.Parse(matrixFormatParams, content)
		}
		file, err := portal.downloadAndDecryptMatrixMedia(ctx, content)
		if err != nil {
			return nil, err
		}
		attachmentPointer, err := signalmeow.UploadAttachment(sender.SignalDevice, file, content.GetInfo().MimeType, fileName)
		if err != nil {
			return nil, err
		}
		outgoingMessage = signalmeow.DataMessageForAttachment(attachmentPointer, caption, ranges)

	case event.MsgLocation:
		fallthrough
	default:
		return nil, fmt.Errorf("%w %q", errUnknownMsgType, content.MsgType)
	}

	// Include a quote if this is a reply
	replyID := content.RelatesTo.GetReplyTo()
	if replyID != "" {
		originalMessage, err := portal.bridge.DB.Message.GetByMXID(ctx, replyID)
		if err != nil {
			return nil, fmt.Errorf("failed to get reply target: %w", err)
		} else if originalMessage != nil {
			signalmeow.AddQuoteToDataMessage(
				outgoingMessage,
				originalMessage.Sender,
				originalMessage.Timestamp,
			)
		} else {
			zerolog.Ctx(ctx).Warn().Str("reply_event_id", replyID.String()).Msg("Reply target not found")
		}
	}
	return outgoingMessage, nil
}

func (portal *Portal) sendSignalMessage(ctx context.Context, msg *signalmeow.SignalContent, sender *User, evtID id.EventID) error {
	recipientSignalID := portal.ChatID
	portal.log.Debug().Msgf("Sending event %s to Signal %s", evtID, recipientSignalID)

	// Check to see if recipientSignalID is a standard UUID (with dashes)
	var err error
	if _, uuidErr := uuid.Parse(recipientSignalID); uuidErr == nil {
		// this is a 1:1 chat
		result := signalmeow.SendMessage(ctx, sender.SignalDevice, recipientSignalID, msg)
		if !result.WasSuccessful {
			err = result.FailedSendResult.Error
			portal.log.Error().Msgf("Error sending event %s to Signal %s: %s", evtID, recipientSignalID, err)
		}
	} else {
		// this is a group chat
		groupID := signalmeow.GroupIdentifier(recipientSignalID)
		result, err := signalmeow.SendGroupMessage(ctx, sender.SignalDevice, groupID, msg)
		if err != nil {
			// check the start of the error string, see if it starts with "No group master key found for group identifier"
			if strings.HasPrefix(err.Error(), "No group master key found for group identifier") {
				portal.MainIntent().SendNotice(portal.MXID, "Missing group encryption key. Please ask a group member to send a message in this chat, then retry sending.")
			}
			portal.log.Error().Msgf("Error sending event %s to Signal group %s: %s", evtID, recipientSignalID, err)
			return err
		}
		totalRecipients := len(result.FailedToSendTo) + len(result.SuccessfullySentTo)
		if len(result.FailedToSendTo) > 0 {
			portal.log.Error().Msgf("Failed to send event %s to %d of %d members of Signal group %s", evtID, len(result.FailedToSendTo), totalRecipients, recipientSignalID)
		}
		if len(result.SuccessfullySentTo) == 0 && len(result.FailedToSendTo) == 0 {
			portal.log.Debug().Msgf("No successes or failures - Probably sent to myself")
		} else if len(result.SuccessfullySentTo) == 0 {
			portal.log.Error().Msgf("Failed to send event %s to all %d members of Signal group %s", evtID, totalRecipients, recipientSignalID)
			err = errors.New("failed to send to any members of Signal group")
		} else if len(result.SuccessfullySentTo) < totalRecipients {
			portal.log.Warn().Msgf("Only sent event %s to %d of %d members of Signal group %s", evtID, len(result.SuccessfullySentTo), totalRecipients, recipientSignalID)
		} else {
			portal.log.Debug().Msgf("Sent event %s to all %d members of Signal group %s", evtID, totalRecipients, recipientSignalID)
		}
	}
	return err
}

func (portal *Portal) sendMessageStatusCheckpointSuccess(evt *event.Event) {
	portal.sendDeliveryReceipt(evt.ID)
	portal.bridge.SendMessageSuccessCheckpoint(evt, status.MsgStepRemote, 0)

	var deliveredTo *[]id.UserID
	if portal.IsPrivateChat() {
		deliveredTo = &[]id.UserID{}
	}
	portal.sendStatusEvent(evt.ID, "", nil, deliveredTo)
}

func (portal *Portal) sendMessageStatusCheckpointFailed(evt *event.Event, err error) {
	portal.sendDeliveryReceipt(evt.ID)
	portal.bridge.SendMessageErrorCheckpoint(evt, status.MsgStepRemote, err, true, 0)
	portal.sendStatusEvent(evt.ID, "", err, nil)
}

func (portal *Portal) handleSignalMessages(portalMessage portalSignalMessage) {
	log := portal.log.With().
		Str("action", "handle signal message").
		Str("sender", portalMessage.sender.SignalID.String()).
		Uint64("timestamp", portalMessage.message.Base().Timestamp).
		Int("part_index", portalMessage.message.Base().PartIndex).
		Logger()
	ctx := log.WithContext(context.TODO())
	if existingMessage, err := portal.bridge.DB.Message.GetBySignalID(
		ctx,
		portalMessage.sender.SignalID,
		portalMessage.message.Base().Timestamp,
		portalMessage.message.Base().PartIndex,
		portal.Receiver,
	); err != nil {
		log.Err(err).Msg("Failed to check if message was already handled")
		return
	} else if existingMessage != nil {
		log.Debug().Msg("Ignoring duplicate message")
		return
	}
	if portal.MXID == "" {
		log.Debug().Msg("Creating Matrix room from incoming message")
		if err := portal.CreateMatrixRoom(portalMessage.user, nil); err != nil {
			log.Error().Err(err).Msg("Failed to create portal room")
			return
		}
		ensureGroupPuppetsAreJoinedToPortal(context.Background(), portalMessage.user, portal)
		signalmeow.SendContactSyncRequest(context.TODO(), portalMessage.user.SignalDevice)
	}

	intent := portalMessage.sender.IntentFor(portal)
	if intent == nil {
		portal.log.Error().Msg("Failed to get message intent")
		return
	}

	var err error
	if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeText {
		err = portal.handleSignalTextMessage(ctx, portalMessage, intent)
		if err != nil {
			portal.log.Error().Err(err).Msg("Failed to handle text message")
			return
		}
	} else if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeAttachment {
		err = portal.handleSignalAttachmentMessage(ctx, portalMessage, intent)
		if err != nil {
			portal.log.Error().Err(err).Msg("Failed to handle attachment message")
			return
		}
	} else if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeReaction {
		portal.handleSignalReactionMessage(ctx, portalMessage, intent)
	} else if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeDelete {
		portal.handleSignalDeleteMessage(ctx, portalMessage, intent)
	} else if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeSticker {
		err := portal.handleSignalStickerMessage(ctx, portalMessage, intent)
		if err != nil {
			portal.log.Error().Err(err).Msg("Failed to handle sticker message")
			return
		}
	} else if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeTyping {
		err := portal.handleSignalTypingMessage(portalMessage, intent)
		if err != nil {
			portal.log.Error().Err(err).Msg("Failed to handle typing message")
			return
		}
	} else if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeReceipt {
		portal.handleSignalReceiptMessage(ctx, portalMessage, intent)
	} else if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeCall {
		err := portal.handleSignalCallMessage(portalMessage, intent)
		if err != nil {
			portal.log.Error().Err(err).Msg("Failed to handle call message")
			return
		}
	} else if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeContactCard {
		err := portal.handleSignalContactCardMessage(portalMessage, intent)
		if err != nil {
			portal.log.Error().Err(err).Msg("Failed to handle contact card message")
			return
		}
	} else if portalMessage.message.MessageType() == signalmeow.IncomingSignalMessageTypeUnhandled {
		err := portal.handleSignalUnhandledMessage(portalMessage, intent)
		if err != nil {
			portal.log.Error().Err(err).Msg("Failed to handle unhandled message")
			return
		}
	} else {
		portal.log.Warn().Msgf("Unknown message type: %v", portalMessage.message.MessageType())
		return
	}
}

func (portal *Portal) storeMessageInDB(ctx context.Context, eventID id.EventID, senderSignalID uuid.UUID, timestamp uint64, partIndex int) {
	dbMessage := portal.bridge.DB.Message.New()
	dbMessage.MXID = eventID
	dbMessage.RoomID = portal.MXID
	dbMessage.Sender = senderSignalID
	dbMessage.Timestamp = timestamp
	dbMessage.PartIndex = partIndex
	dbMessage.SignalChatID = portal.ChatID
	dbMessage.SignalReceiver = portal.Receiver
	err := dbMessage.Insert(ctx)
	if err != nil {
		portal.log.Err(err).Msg("Failed to insert message into database")
	}
}

func (portal *Portal) storeReactionInDB(
	ctx context.Context,
	eventID id.EventID,
	senderSignalID,
	msgAuthor uuid.UUID,
	msgTimestamp uint64,
	emoji string,
) {
	dbReaction := portal.bridge.DB.Reaction.New()
	dbReaction.MXID = eventID
	dbReaction.RoomID = portal.MXID
	dbReaction.SignalChatID = portal.ChatID
	dbReaction.SignalReceiver = portal.Receiver
	dbReaction.Author = senderSignalID
	dbReaction.MsgAuthor = msgAuthor
	dbReaction.MsgTimestamp = msgTimestamp
	dbReaction.Emoji = emoji
	err := dbReaction.Insert(ctx)
	if err != nil {
		portal.log.Err(err).Msg("Failed to insert reaction into database")
	}
}

func (portal *Portal) addSignalQuote(ctx context.Context, content *event.MessageEventContent, quote *signalmeow.IncomingSignalMessageQuoteData) {
	if quote == nil {
		return
	}
	quotedSender, err := uuid.Parse(quote.QuotedSender)
	if err != nil {
		return
	}
	originalMessage, err := portal.bridge.DB.Message.GetBySignalID(
		ctx, quotedSender, quote.QuotedTimestamp, 0, portal.Receiver,
	)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Str("quoted_sender", quote.QuotedSender).Uint64("quoted_timestamp", quote.QuotedTimestamp).Msg("Failed to get quoted message from database")
		return
	} else if originalMessage == nil {
		zerolog.Ctx(ctx).Warn().Str("quoted_sender", quote.QuotedSender).Uint64("quoted_timestamp", quote.QuotedTimestamp).Msg("Quote target message not found")
		return
	}
	content.RelatesTo = &event.RelatesTo{
		InReplyTo: &event.InReplyTo{
			EventID: originalMessage.MXID,
		},
	}
	mentionMXID := portal.bridge.FormatPuppetMXID(originalMessage.Sender)
	user := portal.bridge.GetUserBySignalID(originalMessage.Sender)
	if user != nil {
		mentionMXID = user.MXID
	}
	if !slices.Contains(content.Mentions.UserIDs, mentionMXID) {
		content.Mentions.UserIDs = append(content.Mentions.UserIDs, mentionMXID)
	}
}

func (portal *Portal) addDisappearingMessage(ctx context.Context, eventID id.EventID, expireInSeconds int64, startTimerNow bool) {
	portal.bridge.disappearingMessagesManager.AddDisappearingMessage(ctx, eventID, portal.MXID, time.Duration(expireInSeconds)*time.Second, startTimerNow)
}

var signalFormatParams *signalfmt.FormatParams
var matrixFormatParams *matrixfmt.HTMLParser

func (portal *Portal) handleSignalTextMessage(ctx context.Context, portalMessage portalSignalMessage, intent *appservice.IntentAPI) error {
	timestamp := portalMessage.message.Base().Timestamp
	msg := (portalMessage.message).(signalmeow.IncomingSignalMessageText)
	content := signalfmt.Parse(msg.Content, msg.ContentRanges, signalFormatParams)
	portal.addSignalQuote(ctx, content, msg.Quote)
	resp, err := portal.sendMatrixMessage(intent, event.EventMessage, content, nil, int64(timestamp))
	if err != nil {
		return err
	}
	if resp.EventID == "" {
		return errors.New("Didn't receive event ID from Matrix")
	}
	portal.storeMessageInDB(ctx, resp.EventID, portalMessage.sender.SignalID, timestamp, portalMessage.message.Base().PartIndex)
	portal.addDisappearingMessage(ctx, resp.EventID, portalMessage.message.Base().ExpiresIn, portalMessage.sync)
	return err
}

func (portal *Portal) handleSignalStickerMessage(ctx context.Context, portalMessage portalSignalMessage, intent *appservice.IntentAPI) error {
	timestamp := portalMessage.message.Base().Timestamp
	msg := (portalMessage.message).(signalmeow.IncomingSignalMessageSticker)
	content := &event.MessageEventContent{
		MsgType:  event.MessageType(event.EventSticker.Type),
		Body:     msg.Emoji,
		FileName: msg.Filename,
		Info: &event.FileInfo{
			MimeType: msg.ContentType,
			Width:    int(msg.Width),
			Height:   int(msg.Height),
		},
		Mentions: &event.Mentions{},
	}

	portal.addSignalQuote(ctx, content, msg.Quote)
	err := portal.uploadMediaToMatrix(intent, msg.Sticker, content)
	if err != nil {
		portal.log.Error().Err(err).Msg("Failed to upload media")
	}

	resp, err := portal.sendMatrixMessage(intent, event.EventSticker, content, nil, int64(timestamp))
	if err != nil {
		return err
	}
	if resp.EventID == "" {
		return errors.New("Didn't receive event ID from Matrix")
	}
	portal.storeMessageInDB(ctx, resp.EventID, portalMessage.sender.SignalID, timestamp, portalMessage.message.Base().PartIndex)
	portal.addDisappearingMessage(ctx, resp.EventID, portalMessage.message.Base().ExpiresIn, portalMessage.sync)
	return err
}

func (portal *Portal) handleSignalCallMessage(portalMessage portalSignalMessage, intent *appservice.IntentAPI) error {
	callMessage := (portalMessage.message).(signalmeow.IncomingSignalMessageCall)
	var message string
	if callMessage.IsRinging {
		message = "Incoming Call"
	} else {
		message = "Call Ended"
	}
	portal.MainIntent().SendNotice(portal.MXID, message)
	return nil
}

func (portal *Portal) handleSignalContactCardMessage(portalMessage portalSignalMessage, intent *appservice.IntentAPI) error {
	contactCardMessage := (portalMessage.message).(signalmeow.IncomingSignalMessageContactCard)
	messageParts := []string{}
	messageParts = append(messageParts, contactCardMessage.DisplayName)
	messageParts = append(messageParts, contactCardMessage.Organization)
	for _, phoneNumber := range contactCardMessage.PhoneNumbers {
		messageParts = append(messageParts, phoneNumber)
	}
	for _, email := range contactCardMessage.Emails {
		messageParts = append(messageParts, email)
	}
	for _, address := range contactCardMessage.Addresses {
		messageParts = append(messageParts, address)
	}
	messageParts = slices.DeleteFunc(messageParts, func(s string) bool {
		return s == ""
	})
	message := strings.Join(messageParts, "\n")
	intent.SendNotice(portal.MXID, message)

	return nil
}

func (portal *Portal) handleSignalUnhandledMessage(portalMessage portalSignalMessage, intent *appservice.IntentAPI) error {
	unhandledMessage := (portalMessage.message).(signalmeow.IncomingSignalMessageUnhandled)
	portal.log.Warn().Msgf("Received unhandled message type %s, notice: %s", unhandledMessage.Type, unhandledMessage.Notice)
	notice := unhandledMessage.Notice
	portalMessage.sender.DefaultIntent().SendNotice(portal.MXID, notice)
	return nil
}

func (portal *Portal) handleSignalReceiptMessage(ctx context.Context, portalMessage portalSignalMessage, intent *appservice.IntentAPI) {
	receiptMessage := (portalMessage.message).(signalmeow.IncomingSignalMessageReceipt)
	log := zerolog.Ctx(ctx)
	messageSender, err := uuid.Parse(receiptMessage.OriginalSender)
	// TODO handle err
	timestamp := receiptMessage.OriginalTimestamp
	lastPart, err := portal.bridge.DB.Message.GetLastPartBySignalID(ctx, messageSender, timestamp, portal.Receiver)
	if err != nil {
		log.Err(err).Msg("Failed to get receipt target message")
		return
	} else if lastPart == nil {
		log.Err(err).Msg("Receipt target message not found")
		return
	}

	if receiptMessage.ReceiptType == signalmeow.IncomingSignalMessageReceiptTypeRead {
		log.Debug().Msg("Received read receipt")

		// Don't process read receipts for messages older than the latest one we've seen
		if receiptMessage.OriginalTimestamp <= portal.latestReadTimestamp {
			log.Debug().Msgf("Ignoring read receipt for timestamp %d", receiptMessage.OriginalTimestamp)
			return
		}
		portal.latestReadTimestamp = receiptMessage.OriginalTimestamp

		log.Debug().Msgf("Marking message %s as read", lastPart.MXID)
		err := portal.SetReadMarkers(lastPart, portalMessage.sender)
		if err != nil {
			log.Error().Err(err).Msgf("Failed to set read markers for message %s", lastPart.MXID)
			return
		}
		// TODO only schedule disappearing when user reads from other device
		portal.ScheduleDisappearing()
	} else if receiptMessage.ReceiptType == signalmeow.IncomingSignalMessageReceiptTypeDelivery {
		log.Debug().Msg("Received delivery receipt")
		// Only send delivery MSS for DMs, not groups
		if portal.IsPrivateChat() {
			time := jsontime.UMInt(int64(receiptMessage.Timestamp))
			portal.bridge.SendRawMessageCheckpoint(&status.MessageCheckpoint{
				EventID:    lastPart.MXID,
				RoomID:     portal.MXID,
				Step:       status.MsgStepRemote,
				Timestamp:  time,
				Status:     status.MsgStatusDelivered,
				ReportedBy: status.MsgReportedByBridge,
			})
			portal.sendStatusEvent(lastPart.MXID, "", nil, &[]id.UserID{portal.MainIntent().UserID})
		}
	}
	return
}

func (portal *Portal) SetReadMarkers(dbMessage *database.Message, sender *Puppet) error {
	puppetIntent := sender.IntentFor(portal)
	// Gotta build some custom JSON that isn't in mautrix yet
	type customReadReceipt struct {
		Timestamp          int64  `json:"ts,omitempty"`
		DoublePuppetSource string `json:"fi.mau.double_puppet_source,omitempty"`
	}

	type customReadMarkers struct {
		mautrix.ReqSetReadMarkers
		ReadExtra      customReadReceipt `json:"com.beeper.read.extra"`
		FullyReadExtra customReadReceipt `json:"com.beeper.fully_read.extra"`
	}
	doublePuppet := puppetIntent.IsCustomPuppet
	extra := customReadReceipt{}
	if doublePuppet {
		extra.DoublePuppetSource = portal.bridge.Name
	}
	content := customReadMarkers{
		ReqSetReadMarkers: mautrix.ReqSetReadMarkers{
			Read:      dbMessage.MXID,
			FullyRead: dbMessage.MXID,
		},
		ReadExtra:      extra,
		FullyReadExtra: extra,
	}
	return sender.IntentFor(portal).SetReadMarkers(portal.MXID, content)
}

const SignalTypingTimeout = 15 * time.Second

func (portal *Portal) handleSignalTypingMessage(portalMessage portalSignalMessage, intent *appservice.IntentAPI) error {
	typingMessage := (portalMessage.message).(signalmeow.IncomingSignalMessageTyping)
	var err error
	if typingMessage.IsTyping {
		_, err = intent.UserTyping(portal.MXID, true, SignalTypingTimeout)
	} else {
		_, err = intent.UserTyping(portal.MXID, false, 0)
	}
	return err
}

func typingDiff(prev, new []id.UserID) (started, stopped []id.UserID) {
OuterNew:
	for _, userID := range new {
		for _, previousUserID := range prev {
			if userID == previousUserID {
				continue OuterNew
			}
		}
		started = append(started, userID)
	}
OuterPrev:
	for _, userID := range prev {
		for _, previousUserID := range new {
			if userID == previousUserID {
				continue OuterPrev
			}
		}
		stopped = append(stopped, userID)
	}
	return
}

func (portal *Portal) setTyping(userIDs []id.UserID, isTyping bool) {
	for _, userID := range userIDs {
		user := portal.bridge.GetUserByMXID(userID)
		if user == nil || !user.IsLoggedIn() {
			continue
		}
		recipientSignalID := portal.ChatID

		// Check to see if recipientSignalID is a standard UUID (with dashes)
		// Note: not handling sending to a group right now, since that will
		// require SenderKey sending to not be terrible
		var err error
		if _, uuidErr := uuid.Parse(recipientSignalID); uuidErr == nil {
			// this is a 1:1 chat
			portal.log.Debug().Msgf("Sending Typing event to Signal %s", recipientSignalID)
			ctx := context.Background()
			typingMessage := signalmeow.TypingMessage(isTyping)
			result := signalmeow.SendMessage(ctx, user.SignalDevice, recipientSignalID, typingMessage)
			if !result.WasSuccessful {
				err = result.FailedSendResult.Error
				portal.log.Error().Msgf("Error sending event to Signal %s: %s", recipientSignalID, err)
			}
		}
	}
}

// mautrix-go TypingPortal interface
func (portal *Portal) HandleMatrixTyping(newTyping []id.UserID) {
	portal.currentlyTypingLock.Lock()
	defer portal.currentlyTypingLock.Unlock()
	startedTyping, stoppedTyping := typingDiff(portal.currentlyTyping, newTyping)
	portal.currentlyTyping = newTyping
	portal.setTyping(startedTyping, true)
	portal.setTyping(stoppedTyping, false)
}

// mautrix-go ReadReceiptHandlingPortal interface
func (portal *Portal) HandleMatrixReadReceipt(sender bridge.User, eventID id.EventID, receipt event.ReadReceipt) {
	log := portal.log.With().
		Str("action", "handle matrix read receipt").
		Str("event_id", eventID.String()).
		Str("sender", sender.GetMXID().String()).
		Logger()
	log.Debug().Msg("Received read receipt")
	portal.ScheduleDisappearing()

	// Find event in the DB
	dbMessage, err := portal.bridge.DB.Message.GetByMXID(context.TODO(), eventID)
	if err != nil {
		log.Err(err).Msg("Failed to get read receipt target message")
		return
	} else if dbMessage == nil {
		log.Warn().Msg("Read receipt target message not found")
		return
	}
	// TODO find all messages that haven't been marked as read by the user
	msg := signalmeow.ReadReceptMessageForTimestamps([]uint64{dbMessage.Timestamp})
	receiptDestination := dbMessage.Sender
	receiptSender := sender.(*User)

	// Don't use portal.sendSignalMessage because we're sending this straight to
	// who sent the original message, not the portal's ChatID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := signalmeow.SendMessage(ctx, receiptSender.SignalDevice, receiptDestination.String(), msg)
	if !result.WasSuccessful {
		log.Err(result.FailedSendResult.Error).
			Str("receipt_destination", receiptDestination.String()).
			Msg("Failed to send read receipt to Signal")
	} else {
		log.Debug().Str("receipt_destination", receiptDestination.String()).Msg("Sent read receipt to Signal")
	}
}

func (portal *Portal) handleSignalAttachmentMessage(ctx context.Context, portalMessage portalSignalMessage, intent *appservice.IntentAPI) error {
	timestamp := portalMessage.message.Base().Timestamp
	msg := (portalMessage.message).(signalmeow.IncomingSignalMessageAttachment)
	content := signalfmt.Parse(msg.Caption, msg.CaptionRanges, signalFormatParams)
	content.Info = &event.FileInfo{
		MimeType: msg.ContentType,
		Size:     int(msg.Size),
		Width:    int(msg.Width),
		Height:   int(msg.Height),
		// TODO: bridge blurhash! (needs mautrix-go update)
	}
	content.FileName = msg.Filename
	// Always need a filename, because filename needs to be set and different than body
	// for the body to be interpreted as a caption
	if content.FileName == "" {
		content.FileName = fmt.Sprintf("%d", timestamp)
		content.FileName = content.FileName + "." + strings.Split(msg.ContentType, "/")[1]
	}
	if content.Body == "" {
		content.Body = content.FileName
		content.FileName = ""
	}
	if strings.HasPrefix(msg.ContentType, "image") {
		portal.log.Debug().Msgf("Received image attachment: %s", msg.ContentType)
		content.MsgType = event.MsgImage
	} else if strings.HasPrefix(msg.ContentType, "video") {
		portal.log.Debug().Msgf("Received video attachment: %s", msg.ContentType)
		content.MsgType = event.MsgVideo
	} else if strings.HasPrefix(msg.ContentType, "audio") {
		portal.log.Debug().Msgf("Received audio attachment: %s", msg.ContentType)
		content.MsgType = event.MsgAudio
	} else {
		portal.log.Debug().Msgf("Received file attachment: %s", msg.ContentType)
		content.MsgType = event.MsgFile
	}
	portal.addSignalQuote(ctx, content, msg.Quote)
	err := portal.uploadMediaToMatrix(intent, msg.Attachment, content)
	if err != nil {
		failureMessage := "Failed to bridge media: "
		if errors.Is(err, mautrix.MTooLarge) {
			failureMessage = failureMessage + "homeserver rejected too large file"
		} else if httpErr, ok := err.(mautrix.HTTPError); ok && httpErr.IsStatus(413) {
			failureMessage = failureMessage + "proxy rejected too large file"
		} else {
			failureMessage = failureMessage + fmt.Sprintf("Failed to bridge media: upload failed: %s", err)
		}
		portal.log.Error().Err(err).Msg(failureMessage)
		portal.MainIntent().SendNotice(portal.MXID, failureMessage)
	}
	resp, err := portal.sendMatrixMessage(intent, event.EventMessage, content, nil, int64(timestamp))
	if err != nil {
		return err
	}
	if resp.EventID == "" {
		return errors.New("Didn't receive event ID from Matrix")
	}
	portal.storeMessageInDB(ctx, resp.EventID, portalMessage.sender.SignalID, timestamp, portalMessage.message.Base().PartIndex)
	portal.addDisappearingMessage(ctx, resp.EventID, portalMessage.message.Base().ExpiresIn, portalMessage.sync)
	return err
}

func (portal *Portal) handleSignalReactionMessage(ctx context.Context, portalMessage portalSignalMessage, intent *appservice.IntentAPI) {
	msg := (portalMessage.message).(signalmeow.IncomingSignalMessageReaction)
	matrixEmoji := variationselector.Add(msg.Emoji) // Add variation selector for Matrix

	log := zerolog.Ctx(ctx)
	log.Debug().
		Str("target_message_sender", msg.TargetAuthorUUID).
		Uint64("target_message_timestamp", msg.TargetMessageTimestamp).
		Msg("Received reaction from Signal")
	parsedTargetAuthor, err := uuid.Parse(msg.TargetAuthorUUID)
	// TODO handle err
	senderUUID, err := uuid.Parse(msg.SenderUUID)
	// TODO handle err
	dbMessage, err := portal.bridge.DB.Message.GetBySignalID(ctx, parsedTargetAuthor, msg.TargetMessageTimestamp, 0, portal.Receiver)
	if err != nil {
		log.Err(err).Msg("Failed to get reaction target message")
		return
	} else if dbMessage == nil {
		log.Warn().Msg("Reaction target message not found")
		return
	}
	existingReaction, err := portal.bridge.DB.Reaction.GetBySignalID(
		ctx,
		parsedTargetAuthor,
		msg.TargetMessageTimestamp,
		senderUUID,
		portal.Receiver,
	)
	if err != nil {
		log.Err(err).Msg("Failed to get existing reaction from database")
		return
	}
	if existingReaction != nil {
		_, err = intent.RedactEvent(portal.MXID, existingReaction.MXID, mautrix.ReqRedact{
			TxnID: "mxsg_unreact_" + existingReaction.MXID.String(),
		})
		if err != nil {
			log.Err(err).Msg("Failed to redact reaction")
		}
		// TODO only delete when removing reaction, update row in db when changing
		err = existingReaction.Delete(ctx)
		if err != nil {
			log.Err(err).Msg("Failed to delete reaction from database")
		}
		if msg.Remove {
			return
		}
	} else if msg.Remove {
		log.Warn().Msg("Reaction removal target reaction not found")
		return
	}
	// Create a new message event with the reaction
	content := &event.ReactionEventContent{
		RelatesTo: event.RelatesTo{
			Type:    event.RelAnnotation,
			Key:     matrixEmoji,
			EventID: dbMessage.MXID,
		},
	}
	resp, err := portal.sendMatrixReaction(intent, event.EventReaction, content, nil, 0)
	if err != nil {
		portal.log.Err(err).Msgf("Failed to send reaction: %v", err)
		return
	}

	// Store our new reaction in the DB
	portal.storeReactionInDB(
		ctx,
		resp.EventID,
		portalMessage.sender.SignalID,
		parsedTargetAuthor,
		dbMessage.Timestamp,
		msg.Emoji, // Store without variation selector, as they come from Signal
	)
}

func (portal *Portal) handleSignalDeleteMessage(ctx context.Context, portalMessage portalSignalMessage, intent *appservice.IntentAPI) {
	msg := (portalMessage.message).(signalmeow.IncomingSignalMessageDelete)

	senderUUID, err := uuid.Parse(msg.SenderUUID)
	// TODO handle err

	log := zerolog.Ctx(ctx)
	// Find the event ID of the message to delete
	messages, err := portal.bridge.DB.Message.GetAllPartsBySignalID(ctx, senderUUID, msg.TargetMessageTimestamp, portal.Receiver)
	if err != nil {
		log.Err(err).Msg("Failed to get messages to delete")
		return
	} else if len(messages) == 0 {
		log.Warn().Msg("Didn't find any messages to delete")
		return
	}
	for _, targetMsg := range messages {
		_, err = intent.RedactEvent(portal.MXID, targetMsg.MXID)
		if err != nil {
			log.Err(err).Msg("Failed to redact message")
			continue
		}
		err = targetMsg.Delete(ctx)
		if err != nil {
			log.Err(err).Msg("Failed to delete message from database")
			continue
		}
	}
	return
}

func (portal *Portal) sendMainIntentMessage(content *event.MessageEventContent) (*mautrix.RespSendEvent, error) {
	return portal.sendMatrixMessage(portal.MainIntent(), event.EventMessage, content, nil, 0)
}

func (portal *Portal) encrypt(intent *appservice.IntentAPI, content *event.Content, eventType event.Type) (event.Type, error) {
	if !portal.Encrypted || portal.bridge.Crypto == nil {
		return eventType, nil
	}
	intent.AddDoublePuppetValue(content)
	// TODO maybe the locking should be inside mautrix-go?
	portal.encryptLock.Lock()
	defer portal.encryptLock.Unlock()
	err := portal.bridge.Crypto.Encrypt(portal.MXID, eventType, content)
	if err != nil {
		return eventType, fmt.Errorf("failed to encrypt event: %w", err)
	}
	return event.EventEncrypted, nil
}

func (portal *Portal) encryptFileInPlace(data []byte, mimeType string) (string, *event.EncryptedFileInfo) {
	if !portal.Encrypted {
		return mimeType, nil
	}

	file := &event.EncryptedFileInfo{
		EncryptedFile: *attachment.NewEncryptedFile(),
		URL:           "",
	}
	file.EncryptInPlace(data)
	return "application/octet-stream", file
}

func (portal *Portal) uploadMediaToMatrix(intent *appservice.IntentAPI, data []byte, content *event.MessageEventContent) error {
	uploadMimeType, file := portal.encryptFileInPlace(data, content.Info.MimeType)

	req := mautrix.ReqUploadMedia{
		ContentBytes: data,
		ContentType:  uploadMimeType,
	}
	var mxc id.ContentURI
	if portal.bridge.Config.Homeserver.AsyncMedia {
		uploaded, err := intent.UploadAsync(req)
		if err != nil {
			return err
		}
		mxc = uploaded.ContentURI
	} else {
		uploaded, err := intent.UploadMedia(req)
		if err != nil {
			return err
		}
		mxc = uploaded.ContentURI
	}

	if file != nil {
		file.URL = mxc.CUString()
		content.File = file
	} else {
		content.URL = mxc.CUString()
	}

	content.Info.Size = len(data)
	if content.Info.Width == 0 && content.Info.Height == 0 && strings.HasPrefix(content.Info.MimeType, "image/") {
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(data))
		content.Info.Width, content.Info.Height = cfg.Width, cfg.Height
	}

	// This is a hack for bad clients like Element iOS that require a thumbnail (https://github.com/vector-im/element-ios/issues/4004)
	if strings.HasPrefix(content.Info.MimeType, "image/") && content.Info.ThumbnailInfo == nil {
		infoCopy := *content.Info
		content.Info.ThumbnailInfo = &infoCopy
		if content.File != nil {
			content.Info.ThumbnailFile = file
		} else {
			content.Info.ThumbnailURL = content.URL
		}
	}
	return nil
}

// Boilerplate to send different event types with a modicum of type safety
func (portal *Portal) sendMatrixMessage(intent *appservice.IntentAPI, eventType event.Type, content *event.MessageEventContent, extraContent map[string]interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	return portal.sendMatrixEventContent(intent, eventType, content, extraContent, timestamp)
}
func (portal *Portal) sendMatrixReaction(intent *appservice.IntentAPI, eventType event.Type, content *event.ReactionEventContent, extraContent map[string]interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	return portal.sendMatrixEventContent(intent, eventType, content, extraContent, timestamp)
}
func (portal *Portal) sendMatrixEventContent(intent *appservice.IntentAPI, eventType event.Type, content interface{}, extraContent map[string]interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	wrappedContent := event.Content{Parsed: content, Raw: extraContent}
	if eventType != event.EventReaction {
		var err error
		eventType, err = portal.encrypt(intent, &wrappedContent, eventType)
		if err != nil {
			return nil, err
		}
	}

	_, _ = intent.UserTyping(portal.MXID, false, 0)
	if timestamp == 0 {
		return intent.SendMessageEvent(portal.MXID, eventType, &wrappedContent)
	} else {
		return intent.SendMassagedMessageEvent(portal.MXID, eventType, &wrappedContent, timestamp)
	}
}

func (portal *Portal) getEncryptionEventContent() (evt *event.EncryptionEventContent) {
	evt = &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}
	if rot := portal.bridge.Config.Bridge.Encryption.Rotation; rot.EnableCustom {
		evt.RotationPeriodMillis = rot.Milliseconds
		evt.RotationPeriodMessages = rot.Messages
	}
	return
}

func (portal *Portal) shouldSetDMRoomMetadata() bool {
	return !portal.IsPrivateChat() ||
		portal.bridge.Config.Bridge.PrivateChatPortalMeta == "always" ||
		(portal.IsEncrypted() && portal.bridge.Config.Bridge.PrivateChatPortalMeta != "never")
}

func (portal *Portal) ensureUserInvited(user *User) bool {
	return user.ensureInvited(portal.MainIntent(), portal.MXID, portal.IsPrivateChat())
}

func (portal *Portal) CreateMatrixRoom(user *User, meta *any) error {
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	if portal.MXID != "" {
		portal.log.Debug().Msg("Not creating room: already exists")
		return nil
	}
	portal.log.Debug().Msg("Creating matrix room")

	//meta = portal.UpdateInfo(user, meta)
	//if meta == nil {
	//	return fmt.Errorf("didn't find metadata")
	//}

	intent := portal.MainIntent()

	if err := intent.EnsureRegistered(); err != nil {
		portal.log.Error().Err(err).Msg("failed to ensure registered")
		return err
	}

	bridgeInfoStateKey, bridgeInfo := portal.getBridgeInfo()
	initialState := []*event.Event{{
		Type:     event.StateBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}, {
		// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
		Type:     event.StateHalfShotBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}}

	if !portal.AvatarURL.IsEmpty() {
		initialState = append(initialState, &event.Event{
			Type: event.StateRoomAvatar,
			Content: event.Content{Parsed: &event.RoomAvatarEventContent{
				URL: portal.AvatarURL,
			}},
		})
	}

	creationContent := make(map[string]interface{})
	if !portal.bridge.Config.Bridge.FederateRooms {
		creationContent["m.federate"] = false
	}

	var invite []id.UserID

	if portal.bridge.Config.Bridge.Encryption.Default {
		initialState = append(initialState, &event.Event{
			Type: event.StateEncryption,
			Content: event.Content{
				Parsed: portal.getEncryptionEventContent(),
			},
		})
		portal.Encrypted = true

		if portal.IsPrivateChat() {
			invite = append(invite, portal.bridge.Bot.UserID)
		}
	}

	resp, err := intent.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility:      "private",
		Name:            portal.Name,
		Topic:           portal.Topic,
		Invite:          invite,
		Preset:          "private_chat",
		IsDirect:        portal.IsPrivateChat(),
		InitialState:    initialState,
		CreationContent: creationContent,
	})
	if err != nil {
		portal.log.Warn().Err(err).Msg("failed to create room")
		return err
	}

	portal.NameSet = true
	//portal.TopicSet = true
	portal.AvatarSet = !portal.AvatarURL.IsEmpty()
	portal.MXID = resp.RoomID
	portal.bridge.portalsLock.Lock()
	portal.bridge.portalsByMXID[portal.MXID] = portal
	portal.bridge.portalsLock.Unlock()
	err = portal.Update(context.TODO())
	if err != nil {
		portal.log.Err(err).Msg("Failed to save created portal mxid")
	}
	portal.log.Info().Msgf("Created matrix room %s", portal.MXID)

	if portal.Encrypted && portal.IsPrivateChat() {
		err = portal.bridge.Bot.EnsureJoined(portal.MXID, appservice.EnsureJoinedParams{BotOverride: portal.MainIntent().Client})
		if err != nil {
			portal.log.Error().Err(err).Msg("Failed to ensure bridge bot is joined to private chat portal")
		}
	}

	user.ensureInvited(portal.MainIntent(), portal.MXID, portal.IsPrivateChat())
	user.syncChatDoublePuppetDetails(portal, true)

	//portal.syncParticipants(user, channel.Recipients)

	if portal.IsPrivateChat() {
		portal.log.Debug().Msgf("Portal is private chat, updating direct chats: %s", portal.MXID)
		puppet := user.bridge.GetPuppetBySignalID(portal.Receiver)
		if puppet == nil {
			portal.log.Error().Msgf("Failed to find puppet for portal receiver %s", portal.Receiver)
			return nil
		}

		chats := map[id.UserID][]id.RoomID{puppet.MXID: {portal.MXID}}
		user.UpdateDirectChats(chats)
	}

	return nil
}

func (portal *Portal) UpdateInfo(user *User, meta *any) *any {
	return nil
}

// ** Portal loading and fetching **
var (
	portalCreationDummyEvent = event.Type{Type: "fi.mau.dummy.portal_created", Class: event.MessageEventType}
)

func (br *SignalBridge) loadPortal(ctx context.Context, dbPortal *database.Portal, key *database.PortalKey) *Portal {
	if dbPortal == nil {
		if key == nil {
			return nil
		}

		dbPortal = br.DB.Portal.New()
		dbPortal.PortalKey = *key
		err := dbPortal.Insert(ctx)
		if err != nil {
			br.ZLog.Err(err).Msg("Failed to insert new portal")
			return nil
		}
	}

	portal := br.NewPortal(dbPortal)

	br.portalsByID[portal.PortalKey] = portal
	if portal.MXID != "" {
		br.portalsByMXID[portal.MXID] = portal
	}

	return portal
}

func (br *SignalBridge) GetPortalByMXID(mxid id.RoomID) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()

	portal, ok := br.portalsByMXID[mxid]
	if !ok {
		dbPortal, err := br.DB.Portal.GetByMXID(context.TODO(), mxid)
		if err != nil {
			br.ZLog.Err(err).Msg("Failed to get portal from database")
			return nil
		}
		return br.loadPortal(context.TODO(), dbPortal, nil)
	}

	return portal
}

func (br *SignalBridge) GetPortalByChatID(key database.PortalKey) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	// If this PortalKey is for a group, Receiver should be empty
	if key.UserID() == uuid.Nil {
		key.Receiver = uuid.Nil
	}
	portal, ok := br.portalsByID[key]
	if !ok {
		dbPortal, err := br.DB.Portal.GetByChatID(context.TODO(), key)
		if err != nil {
			br.ZLog.Err(err).Msg("Failed to get portal from database")
			return nil
		}
		return br.loadPortal(context.TODO(), dbPortal, &key)
	}
	return portal
}

func (portal *Portal) getBridgeInfoStateKey() string {
	return fmt.Sprintf("net.maunium.signal://signal/%s", portal.ChatID)
}

// ** DisappearingPortal interface **
func (portal *Portal) ScheduleDisappearing() {
	portal.bridge.disappearingMessagesManager.ScheduleDisappearingForRoom(context.TODO(), portal.MXID)
}

func (portal *Portal) HandleNewDisappearingMessageTime(newTimer uint32) {
	portal.log.Debug().Msgf("Disappearing message timer changed to %d", newTimer)
	intent := portal.bridge.Bot
	if newTimer == 0 {
		intent.SendNotice(portal.MXID, "Disappearing messages disabled")
	} else {
		intent.SendNotice(portal.MXID, fmt.Sprintf("Disappearing messages set to %s", exfmt.Duration(time.Duration(newTimer)*time.Second)))
	}
}

func (portal *Portal) HasRelaybot() bool {
	return portal.bridge.Config.Bridge.Relay.Enabled && len(portal.RelayUserID) > 0
}

func (portal *Portal) addRelaybotFormat(userID id.UserID, content *event.MessageEventContent) bool {
	member := portal.MainIntent().Member(portal.MXID, userID)
	if member == nil {
		member = &event.MemberEventContent{}
	}
	content.EnsureHasHTML()
	data, err := portal.bridge.Config.Bridge.Relay.FormatMessage(content, userID, *member)
	if err != nil {
		portal.log.Err(err).Msg("Failed to apply relaybot format")
	}
	content.FormattedBody = data
	return true
}

func (portal *Portal) Delete() {
	err := portal.Portal.Delete(context.TODO())
	if err != nil {
		portal.log.Err(err).Msg("Failed to delete portal from db")
	}
	portal.bridge.portalsLock.Lock()
	delete(portal.bridge.portalsByID, portal.PortalKey)
	if len(portal.MXID) > 0 {
		delete(portal.bridge.portalsByMXID, portal.MXID)
	}
	//portal.resetChildSpaceStatus()
	portal.bridge.portalsLock.Unlock()
}

func (portal *Portal) Cleanup(puppetsOnly bool) {
	portal.bridge.CleanupRoom(&portal.log, portal.MainIntent(), portal.MXID, puppetsOnly)
}

func (br *SignalBridge) CleanupRoom(log *zerolog.Logger, intent *appservice.IntentAPI, mxid id.RoomID, puppetsOnly bool) {
	if len(mxid) == 0 {
		return
	}
	if br.SpecVersions.Supports(mautrix.BeeperFeatureRoomYeeting) {
		err := intent.BeeperDeleteRoom(mxid)
		if err == nil || errors.Is(err, mautrix.MNotFound) {
			return
		}
		log.Warn().Err(err).Msg("Failed to delete room using beeper yeet endpoint, falling back to normal behavior")
	}
	members, err := intent.JoinedMembers(mxid)
	if err != nil {
		log.Err(err).Msg("Failed to get portal members for cleanup")
		return
	}
	for member := range members.Joined {
		if member == intent.UserID {
			continue
		}
		puppet := br.GetPuppetByMXID(member)
		if puppet != nil {
			_, err = puppet.DefaultIntent().LeaveRoom(mxid)
			if err != nil {
				log.Err(err).Msg("Failed to leave as puppet while cleaning up portal")
			}
		} else if !puppetsOnly {
			_, err = intent.KickUser(mxid, &mautrix.ReqKickUser{UserID: member, Reason: "Deleting portal"})
			if err != nil {
				log.Err(err).Msg("Failed to kick user while cleaning up portal")
			}
		}
	}
	_, err = intent.LeaveRoom(mxid)
	if err != nil {
		log.Err(err).Msg("Failed to leave room while cleaning up portal")
	}
}
