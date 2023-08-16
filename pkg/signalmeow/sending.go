package signalmeow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	signalpb "go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/web"
	"google.golang.org/protobuf/proto"
)

// Sending

func senderCertificate(d *Device) (*libsignalgo.SenderCertificate, error) {
	if d.Connection.SenderCertificate != nil {
		// TODO: check for expired certificate
		return d.Connection.SenderCertificate, nil
	}

	username, password := d.Data.BasicAuthCreds()
	opts := &web.HTTPReqOpt{Username: &username, Password: &password}
	resp, err := web.SendHTTPRequest("GET", "/v1/certificate/delivery", opts)
	defer resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP error: %v", resp.StatusCode)
	}

	type response struct {
		Base64Certificate string `json:"certificate"`
	}
	var r response
	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return nil, err
	}
	rawCertificate, err := base64.StdEncoding.DecodeString(r.Base64Certificate)
	if err != nil {
		return nil, err
	}
	cert, err := libsignalgo.DeserializeSenderCertificate([]byte(rawCertificate))
	d.Connection.SenderCertificate = cert
	return cert, err
}

type MyMessage struct {
	Type                      int    `json:"type"`
	DestinationDeviceID       int    `json:"destinationDeviceId"`
	DestinationRegistrationID int    `json:"destinationRegistrationId"`
	Content                   string `json:"content"`
}

type MyMessages struct {
	Timestamp int64       `json:"timestamp"`
	Online    bool        `json:"online"`
	Urgent    bool        `json:"urgent"`
	Messages  []MyMessage `json:"messages"`
}

func padBlock(block *[]byte, pos int) error {
	if pos >= len(*block) {
		return errors.New("Padding error: position exceeds block length")
	}

	(*block)[pos] = 0x80
	for i := pos + 1; i < len(*block); i++ {
		(*block)[i] = 0
	}

	return nil
}

func addPadding(version uint32, contents []byte) ([]byte, error) {
	if version < 2 {
		return nil, fmt.Errorf("Unknown version %d", version)
	} else if version == 2 {
		return contents, nil
	} else {
		messageLength := len(contents)
		messageLengthWithTerminator := len(contents) + 1
		messagePartCount := messageLengthWithTerminator / 160
		if messageLengthWithTerminator%160 != 0 {
			messagePartCount++
		}

		messageLengthWithPadding := messagePartCount * 160

		buffer := make([]byte, messageLengthWithPadding)
		copy(buffer[:messageLength], contents)

		err := padBlock(&buffer, messageLength)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("Invalid message padding: %v", err))
		}
		return buffer, nil
	}
}

func checkForErrorWithSessions(err error, addresses []*libsignalgo.Address, sessionRecords []*libsignalgo.SessionRecord) error {
	if err != nil {
		return err
	}
	if addresses == nil || sessionRecords == nil {
		return fmt.Errorf("Addresses or session records are nil")
	}
	if len(addresses) != len(sessionRecords) {
		return fmt.Errorf("Mismatched number of addresses (%d) and session records (%d)", len(addresses), len(sessionRecords))
	}
	if len(addresses) == 0 || len(sessionRecords) == 0 {
		return fmt.Errorf("No addresses or session records")
	}
	return nil
}

func howManyOtherDevicesDoWeHave(ctx context.Context, d *Device) int {
	addresses, _, err := d.SessionStoreExtras.AllSessionsForUUID(d.Data.AciUuid, ctx)
	if err != nil {
		return 0
	}
	// Filter out our deviceID
	otherDevices := 0
	for _, address := range addresses {
		deviceID, err := address.DeviceID()
		if err != nil {
			zlog.Err(err).Msg("Error getting deviceID from address")
			continue
		}
		if deviceID != uint(d.Data.DeviceId) {
			otherDevices++
		}
	}
	return otherDevices
}

func buildMessagesToSend(ctx context.Context, d *Device, recipientUuid string, content *signalpb.Content, unauthenticated bool) ([]MyMessage, error) {
	messages := []MyMessage{}

	addresses, sessionRecords, err := d.SessionStoreExtras.AllSessionsForUUID(recipientUuid, ctx)
	if err == nil && (len(addresses) == 0 || len(sessionRecords) == 0) {
		// No sessions, make one with prekey
		FetchAndProcessPreKey(ctx, d, recipientUuid, -1)
		addresses, sessionRecords, err = d.SessionStoreExtras.AllSessionsForUUID(recipientUuid, ctx)
	}
	err = checkForErrorWithSessions(err, addresses, sessionRecords)
	if err != nil {
		return nil, err
	}

	for i, recipientAddress := range addresses {
		recipientDeviceID, err := recipientAddress.DeviceID()
		if err != nil {
			return nil, err
		}

		// Build message payload
		serializedMessage, err := proto.Marshal(content)
		if err != nil {
			return nil, err
		}
		paddedMessage, err := addPadding(3, []byte(serializedMessage)) // TODO: figure out how to get actual version
		sessionRecord := sessionRecords[i]

		var envelopeType int
		var encryptedPayload []byte
		if unauthenticated {
			envelopeType, encryptedPayload, err = buildSSMessageToSend(ctx, d, recipientAddress, paddedMessage)
		} else {
			envelopeType, encryptedPayload, err = buildAuthedMessageToSend(ctx, d, recipientAddress, paddedMessage)
		}

		destinationRegistrationID, err := sessionRecord.GetRemoteRegistrationID()
		if err != nil {
			return nil, err
		}
		outgoingMessage := MyMessage{
			Type:                      envelopeType,
			DestinationDeviceID:       int(recipientDeviceID),
			DestinationRegistrationID: int(destinationRegistrationID),
			Content:                   base64.StdEncoding.EncodeToString(encryptedPayload),
		}
		messages = append(messages, outgoingMessage)
	}

	return messages, nil
}

func buildAuthedMessageToSend(ctx context.Context, d *Device, recipientAddress *libsignalgo.Address, paddedMessage []byte) (envelopeType int, encryptedPayload []byte, err error) {
	cipherTextMessage, err := libsignalgo.Encrypt(
		[]byte(paddedMessage),
		recipientAddress,
		d.SessionStore,
		d.IdentityStore,
		libsignalgo.NewCallbackContext(ctx),
	)
	encryptedPayload, err = cipherTextMessage.Serialize()
	if err != nil {
		return 0, nil, err
	}

	// OMG Signal are you serious why can't your magic numbers just align
	cipherMessageType, _ := cipherTextMessage.MessageType()
	if cipherMessageType == libsignalgo.CiphertextMessageTypePreKey { // 3 -> 3
		envelopeType = int(signalpb.Envelope_PREKEY_BUNDLE)
	} else if cipherMessageType == libsignalgo.CiphertextMessageTypeWhisper { // 2 -> 1
		envelopeType = int(signalpb.Envelope_CIPHERTEXT)
	} else {
		return 0, nil, fmt.Errorf("Unknown message type: %v", cipherMessageType)
	}
	return envelopeType, encryptedPayload, nil
}

func buildSSMessageToSend(ctx context.Context, d *Device, recipientAddress *libsignalgo.Address, paddedMessage []byte) (envelopeType int, encryptedPayload []byte, err error) {
	cert, err := senderCertificate(d)
	if err != nil {
		return 0, nil, err
	}
	encryptedPayload, err = libsignalgo.SealedSenderEncryptPlaintext(
		[]byte(paddedMessage),
		recipientAddress,
		cert,
		d.SessionStore,
		d.IdentityStore,
		libsignalgo.NewCallbackContext(ctx),
	)
	envelopeType = int(signalpb.Envelope_UNIDENTIFIED_SENDER)

	return envelopeType, encryptedPayload, nil
}

type SuccessfulSendResult struct {
	RecipientUuid string
	Unidentified  bool
}
type FailedSendResult struct {
	RecipientUuid string
	Error         error
}
type SendMessageResult struct {
	WasSuccessful bool
	*SuccessfulSendResult
	*FailedSendResult
}
type GroupMessageSendResult struct {
	SuccessfullySentTo []SuccessfulSendResult
	FailedToSendTo     []FailedSendResult
}

func dataMessageFromText(text string, timestamp uint64) *signalpb.DataMessage {
	return &signalpb.DataMessage{
		Body:      proto.String(text),
		Timestamp: &timestamp,
	}
}
func contentFromDataMessage(dataMessage *signalpb.DataMessage) *signalpb.Content {
	return &signalpb.Content{
		DataMessage: dataMessage,
	}
}
func syncMessageFromGroupDataMessage(dataMessage *signalpb.DataMessage, results []SuccessfulSendResult) *signalpb.Content {
	unidentifiedStatuses := []*signalpb.SyncMessage_Sent_UnidentifiedDeliveryStatus{}
	for _, result := range results {
		unidentifiedStatuses = append(unidentifiedStatuses, &signalpb.SyncMessage_Sent_UnidentifiedDeliveryStatus{
			DestinationUuid: &result.RecipientUuid,
			Unidentified:    &result.Unidentified,
		})
	}
	return &signalpb.Content{
		SyncMessage: &signalpb.SyncMessage{
			Sent: &signalpb.SyncMessage_Sent{
				Message:            dataMessage,
				Timestamp:          dataMessage.Timestamp,
				UnidentifiedStatus: unidentifiedStatuses,
			},
		},
	}
}

func syncMessageFromSoloDataMessage(dataMessage *signalpb.DataMessage, result SuccessfulSendResult) *signalpb.Content {
	return &signalpb.Content{
		SyncMessage: &signalpb.SyncMessage{
			Sent: &signalpb.SyncMessage_Sent{
				Message:         dataMessage,
				DestinationUuid: &result.RecipientUuid,
				Timestamp:       dataMessage.Timestamp,
				UnidentifiedStatus: []*signalpb.SyncMessage_Sent_UnidentifiedDeliveryStatus{
					{
						DestinationUuid: &result.RecipientUuid,
						Unidentified:    &result.Unidentified,
					},
				},
			},
		},
	}
}

func SendGroupMessage(ctx context.Context, device *Device, groupID GroupID, text string) (*GroupMessageSendResult, error) {
	group, err := RetrieveGroupByID(ctx, device, groupID)
	if err != nil {
		return nil, err
	}

	// Assemble the content to send
	messageTimestamp := currentMessageTimestamp()
	dataMessage := dataMessageFromText(text, messageTimestamp)
	dataMessage.GroupV2 = groupMetadataForDataMessage(*group)
	content := &signalpb.Content{
		DataMessage: dataMessage,
	}

	// Send to each member of the group
	result := &GroupMessageSendResult{
		SuccessfullySentTo: []SuccessfulSendResult{},
		FailedToSendTo:     []FailedSendResult{},
	}
	for _, member := range group.Members {
		if member.UserId == device.Data.AciUuid {
			// Don't send normal DataMessages to ourselves
			continue
		}
		sentUnidentified, err := sendContent(ctx, device, member.UserId, messageTimestamp, content, 0)
		if err != nil {
			result.FailedToSendTo = append(result.FailedToSendTo, FailedSendResult{
				RecipientUuid: member.UserId,
				Error:         err,
			})
			zlog.Err(err).Msgf("Failed to send to %v", member.UserId)
		} else {
			result.SuccessfullySentTo = append(result.SuccessfullySentTo, SuccessfulSendResult{
				RecipientUuid: member.UserId,
				Unidentified:  sentUnidentified,
			})
			zlog.Trace().Msgf("Successfully sent to %v", member.UserId)
		}
	}

	// No need to send to ourselves if we don't have any other devices
	if howManyOtherDevicesDoWeHave(ctx, device) > 0 {
		syncContent := syncMessageFromGroupDataMessage(dataMessage, result.SuccessfullySentTo)
		_, selfSendErr := sendContent(ctx, device, device.Data.AciUuid, messageTimestamp, syncContent, 0)
		if selfSendErr != nil {
			zlog.Err(selfSendErr).Msg("Failed to send sync message to myself (%v)")
		}
	}

	return result, nil
}

func SendMessage(ctx context.Context, device *Device, recipientUuid string, text string) SendMessageResult {
	// Assemble the content to send
	messageTimestamp := currentMessageTimestamp()
	dataMessage := dataMessageFromText(text, messageTimestamp)
	content := &signalpb.Content{
		DataMessage: dataMessage,
	}

	// Send to the recipient
	sentUnidentified, err := sendContent(ctx, device, recipientUuid, messageTimestamp, content, 0)
	if err != nil {
		return SendMessageResult{
			WasSuccessful: false,
			FailedSendResult: &FailedSendResult{
				RecipientUuid: recipientUuid,
				Error:         err,
			},
		}
	}
	result := SendMessageResult{
		WasSuccessful: true,
		SuccessfulSendResult: &SuccessfulSendResult{
			RecipientUuid: recipientUuid,
			Unidentified:  sentUnidentified,
		},
	}

	// TODO: don't fetch every time
	// (But for now this makes sure we know about all our other devices)
	FetchAndProcessPreKey(ctx, device, device.Data.AciUuid, -1)

	// If we have other devices, send to them too
	if howManyOtherDevicesDoWeHave(ctx, device) > 0 {
		syncContent := syncMessageFromSoloDataMessage(dataMessage, *result.SuccessfulSendResult)
		_, selfSendErr := sendContent(ctx, device, device.Data.AciUuid, messageTimestamp, syncContent, 0)
		if selfSendErr != nil {
			zlog.Err(selfSendErr).Msg("Failed to send sync message to myself")
		}
	}
	return result
}

func currentMessageTimestamp() uint64 {
	return uint64(time.Now().UnixMilli())
}

func sendContent(
	ctx context.Context,
	d *Device,
	recipientUuid string,
	messageTimestamp uint64,
	content *signalpb.Content,
	retryCount int, // For ending recursive retries
) (sentUnidentified bool, err error) {
	// TODO: also handle non sealed-sender messages
	// TODO: also handle pre-key messages (for the aformentioned session establishment)
	// TODO: function returns before message is sent - need async status to caller

	//unidentifiedAccessKey := "a key" // TODO: derive key from their profile key

	if retryCount > 3 {
		err := fmt.Errorf("Too many retries")
		zlog.Err(err).Msgf("sendContent too many retries: %v", retryCount)
		return false, err
	}

	useUnidentifiedSender := true
	profileKey, err := ProfileKeyForSignalID(ctx, d, recipientUuid)
	if err != nil || profileKey == nil {
		zlog.Err(err).Msg("Error getting profile key")
		useUnidentifiedSender = false
	}
	accessKey, err := profileKey.DeriveAccessKey()
	if err != nil {
		zlog.Err(err).Msg("Error deriving access key")
		useUnidentifiedSender = false
	}
	// TODO: JUST FOR DEBUGGING
	//if content.DataMessage != nil {
	//	if *content.DataMessage.Body == "UNSEAL" {
	//		useUnidentifiedSender = false
	//	}
	//}

	// Encrypt messages
	var messages []MyMessage
	messages, err = buildMessagesToSend(ctx, d, recipientUuid, content, useUnidentifiedSender)
	if err != nil {
		zlog.Err(err).Msg("Error building messages to send")
		return false, err
	}

	outgoingMessages := MyMessages{
		Timestamp: int64(messageTimestamp),
		Online:    false,
		Urgent:    true,
		Messages:  messages,
	}
	jsonBytes, err := json.Marshal(outgoingMessages)
	if err != nil {
		return false, err
	}
	path := fmt.Sprintf("/v1/messages/%v", recipientUuid)
	request := web.CreateWSRequest("PUT", path, jsonBytes, nil, nil)

	var responseChan <-chan *signalpb.WebSocketResponseMessage
	if useUnidentifiedSender {
		zlog.Trace().Msgf("Sending message to %v over unidentified WS", recipientUuid)
		base64AccessKey := base64.StdEncoding.EncodeToString(accessKey[:])
		request.Headers = append(request.Headers, "unidentified-access-key:"+base64AccessKey)
		responseChan, err = d.Connection.UnauthedWS.SendRequest(ctx, request)
	} else {
		zlog.Trace().Msgf("Sending message to %v over authed WS", recipientUuid)
		responseChan, err = d.Connection.AuthedWS.SendRequest(ctx, request)
	}
	sentUnidentified = useUnidentifiedSender
	if err != nil {
		return sentUnidentified, err
	}

	response := <-responseChan
	zlog.Trace().Msgf("Received a response to a message send from: %v, id: %v, code: %v", recipientUuid, *response.Id, *response.Status)

	retryableStatuses := []uint32{409, 410, 428, 500, 503}

	// Check to see if our status is retryable
	needToRetry := false
	for _, status := range retryableStatuses {
		if *response.Status == status {
			needToRetry = true
			break
		}
	}

	if needToRetry {
		var err error
		if *response.Status == 409 {
			err = handle409(ctx, d, recipientUuid, response)
		} else if *response.Status == 410 {
			err = handle410(ctx, d, recipientUuid, response)
		} else if *response.Status == 428 {
			err = handle428(ctx, d, recipientUuid, response)
		}
		if err != nil {
			return false, err
		}
		// Try to send again (**RECURSIVELY**)
		sentUnidentified, err = sendContent(ctx, d, recipientUuid, messageTimestamp, content, retryCount+1)
		if err != nil {
			zlog.Err(err).Msg("2nd try sendMessage error")
			return sentUnidentified, err
		}
	} else if *response.Status != 200 {
		err := fmt.Errorf("Unexpected status code while sending: %v", *response.Status)
		zlog.Err(err).Msg("")
		return sentUnidentified, err
	}

	return sentUnidentified, nil
}

// A 409 means our device list was out of date, so we will fix it up
func handle409(ctx context.Context, device *Device, recipientUuid string, response *signalpb.WebSocketResponseMessage) error {
	// Decode json body
	var body map[string]interface{}
	err := json.Unmarshal(response.Body, &body)
	if err != nil {
		zlog.Err(err).Msg("Unmarshal error")
		return err
	}
	// check for missingDevices and extraDevices
	if body["missingDevices"] != nil {
		missingDevices := body["missingDevices"].([]interface{})
		zlog.Debug().Msgf("missing devices found in 409 response: %v", missingDevices)
		// TODO: establish session with missing devices
		for _, missingDevice := range missingDevices {
			FetchAndProcessPreKey(ctx, device, recipientUuid, int(missingDevice.(float64)))
		}
	}
	if body["extraDevices"] != nil {
		extraDevices := body["extraDevices"].([]interface{})
		zlog.Debug().Msgf("extra devices found in 409 response: %v", extraDevices)
		for _, extraDevice := range extraDevices {
			// Remove extra device from the sessionstore
			recipient, err := libsignalgo.NewAddress(
				recipientUuid,
				uint(extraDevice.(float64)),
			)
			if err != nil {
				zlog.Err(err).Msg("NewAddress error")
				return err
			}
			err = device.SessionStoreExtras.RemoveSession(recipient, ctx)
			if err != nil {
				zlog.Err(err).Msg("RemoveSession error")
				return err
			}
		}
	}
	return err
}

// A 410 means we have a stale device, so get rid of it
func handle410(ctx context.Context, device *Device, recipientUuid string, response *signalpb.WebSocketResponseMessage) error {
	// Decode json body
	var body map[string]interface{}
	err := json.Unmarshal(response.Body, &body)
	if err != nil {
		zlog.Err(err).Msg("Unmarshal error")
		return err
	}
	// check for staleDevices and make new sessions with them
	if body["staleDevices"] != nil {
		staleDevices := body["staleDevices"].([]interface{})
		zlog.Debug().Msgf("stale devices found in 410 response: %v", staleDevices)
		for _, staleDevice := range staleDevices {
			recipient, err := libsignalgo.NewAddress(
				recipientUuid,
				uint(staleDevice.(float64)),
			)
			err = device.SessionStoreExtras.RemoveSession(recipient, ctx)
			if err != nil {
				zlog.Err(err).Msg("RemoveSession error")
				return err
			}
			FetchAndProcessPreKey(ctx, device, recipientUuid, int(staleDevice.(float64)))
		}
	}
	return err
}

// We got rate limited.
// We ~~will~~ could try sending a "pushChallenge" response, but if that doesn't work we just gotta wait.
// TODO: explore captcha response
func handle428(ctx context.Context, device *Device, recipientUuid string, response *signalpb.WebSocketResponseMessage) error {
	// Decode json body
	var body map[string]interface{}
	err := json.Unmarshal(response.Body, &body)
	if err != nil {
		zlog.Err(err).Msg("Unmarshal error")
		return err
	}

	// Sample response:
	//id:25 status:428 message:"Precondition Required" headers:"Retry-After:86400"
	//headers:"Content-Type:application/json" headers:"Content-Length:88"
	//body:"{\"token\":\"07af0d73-e05d-42c3-9634-634922061966\",\"options\":[\"recaptcha\",\"pushChallenge\"]}"
	var retryAfterSeconds uint64 = 0
	// Find retry after header
	for _, header := range response.Headers {
		key, value := strings.Split(header, ":")[0], strings.Split(header, ":")[1]
		if key == "Retry-After" {
			retryAfterSeconds, err = strconv.ParseUint(value, 10, 64)
			if err != nil {
				zlog.Err(err).Msg("ParseUint error")
			}
		}
	}
	if retryAfterSeconds > 0 {
		zlog.Warn().Msgf("Got rate limited, need to wait %v seconds", retryAfterSeconds)
	}
	// TODO: responding to a pushChallenge this way doesn't work, server just returns 422
	// Luckily challenges seem rare when sending with sealed sender
	//if body["options"] != nil {
	//	options := body["options"].([]interface{})
	//	for _, option := range options {
	//		if option == "pushChallenge" {
	//			zlog.Info().Msg("Got pushChallenge, sending response")
	//			token := body["token"].(string)
	//			username, password := device.Data.BasicAuthCreds()
	//			response, err := web.SendHTTPRequest(
	//				"PUT",
	//				"/v1/challenge",
	//				&web.HTTPReqOpt{
	//					Body:     []byte(fmt.Sprintf("{\"token\":\"%v\",\"type\":\"pushChallenge\"}", token)),
	//					Username: &username,
	//					Password: &password,
	//				},
	//			)
	//			if err != nil {
	//				zlog.Err(err).Msg("SendHTTPRequest error")
	//				return err
	//			}
	//			if response.StatusCode != 200 {
	//				zlog.Info().Msg("Unexpected status code: %v", response.StatusCode)
	//				return fmt.Errorf("Unexpected status code: %v", response.StatusCode)
	//			}
	//		}
	//	}
	//}
	return nil
}
