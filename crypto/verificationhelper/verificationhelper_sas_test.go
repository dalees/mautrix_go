package verificationhelper_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/maps"

	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestVerification_SAS(t *testing.T) {
	ctx := log.Logger.WithContext(context.TODO())

	testCases := []struct {
		sendingGeneratedCrossSigningKeys bool
		sendingStartsSAS                 bool
		sendingConfirmsFirst             bool
	}{
		{true, true, true},
		{true, true, false},
		{true, false, true},
		{true, false, false},
		{false, true, true},
		{false, true, false},
		{false, false, true},
		{false, false, false},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("sendingGenerated=%t sendingStartsSAS=%t sendingConfirmsFirst=%t", tc.sendingGeneratedCrossSigningKeys, tc.sendingStartsSAS, tc.sendingConfirmsFirst), func(t *testing.T) {
			ts, sendingClient, receivingClient, _, _, sendingMachine, receivingMachine := initServerAndLoginTwoAlice(t, ctx)
			defer ts.Close()
			sendingCallbacks, receivingCallbacks, sendingHelper, receivingHelper := initDefaultCallbacks(t, ctx, sendingClient, receivingClient, sendingMachine, receivingMachine)
			var err error

			var sendingRecoveryKey, receivingRecoveryKey string
			var sendingCrossSigningKeysCache, receivingCrossSigningKeysCache *crypto.CrossSigningKeysCache

			if tc.sendingGeneratedCrossSigningKeys {
				sendingRecoveryKey, sendingCrossSigningKeysCache, err = sendingMachine.GenerateAndUploadCrossSigningKeys(ctx, nil, "")
				require.NoError(t, err)
				assert.NotEmpty(t, sendingRecoveryKey)
				assert.NotNil(t, sendingCrossSigningKeysCache)
			} else {
				receivingRecoveryKey, receivingCrossSigningKeysCache, err = receivingMachine.GenerateAndUploadCrossSigningKeys(ctx, nil, "")
				require.NoError(t, err)
				assert.NotEmpty(t, receivingRecoveryKey)
				assert.NotNil(t, receivingCrossSigningKeysCache)
			}

			// Send the verification request from the sender device and accept
			// it on the receiving device and receive the verification ready
			// event on the sending device.
			txnID, err := sendingHelper.StartVerification(ctx, aliceUserID)
			require.NoError(t, err)
			ts.dispatchToDevice(t, ctx, receivingClient)
			err = receivingHelper.AcceptVerification(ctx, txnID)
			require.NoError(t, err)
			ts.dispatchToDevice(t, ctx, sendingClient)

			// Test that the start event is correct
			var startEvt *event.VerificationStartEventContent
			if tc.sendingStartsSAS {
				err = sendingHelper.StartSAS(ctx, txnID)
				require.NoError(t, err)

				// Ensure that the receiving device received a verification
				// start event.
				receivingInbox := ts.DeviceInbox[aliceUserID][receivingDeviceID]
				assert.Len(t, receivingInbox, 1)
				startEvt = receivingInbox[0].Content.AsVerificationStart()
				assert.Equal(t, sendingDeviceID, startEvt.FromDevice)
			} else {
				err = receivingHelper.StartSAS(ctx, txnID)
				require.NoError(t, err)

				// Ensure that the receiving device received a verification
				// start event.
				sendingInbox := ts.DeviceInbox[aliceUserID][sendingDeviceID]
				assert.Len(t, sendingInbox, 1)
				startEvt = sendingInbox[0].Content.AsVerificationStart()
				assert.Equal(t, receivingDeviceID, startEvt.FromDevice)
			}
			assert.Equal(t, txnID, startEvt.TransactionID)
			assert.Equal(t, event.VerificationMethodSAS, startEvt.Method)
			assert.Contains(t, startEvt.Hashes, event.VerificationHashMethodSHA256)
			assert.Contains(t, startEvt.KeyAgreementProtocols, event.KeyAgreementProtocolCurve25519HKDFSHA256)
			assert.Contains(t, startEvt.MessageAuthenticationCodes, event.MACMethodHKDFHMACSHA256)
			assert.Contains(t, startEvt.MessageAuthenticationCodes, event.MACMethodHKDFHMACSHA256V2)
			assert.Contains(t, startEvt.ShortAuthenticationString, event.SASMethodDecimal)
			assert.Contains(t, startEvt.ShortAuthenticationString, event.SASMethodEmoji)

			// Test that the accept event is correct
			var acceptEvt *event.VerificationAcceptEventContent
			if tc.sendingStartsSAS {
				// Process the verification start event on the receiving
				// device.
				ts.dispatchToDevice(t, ctx, receivingClient)

				// Receiving device sent the accept event to the sending device
				sendingInbox := ts.DeviceInbox[aliceUserID][sendingDeviceID]
				assert.Len(t, sendingInbox, 1)
				acceptEvt = sendingInbox[0].Content.AsVerificationAccept()
			} else {
				// Process the verification start event on the sending device.
				ts.dispatchToDevice(t, ctx, sendingClient)

				// Sending device sent the accept event to the receiving device
				receivingInbox := ts.DeviceInbox[aliceUserID][receivingDeviceID]
				assert.Len(t, receivingInbox, 1)
				acceptEvt = receivingInbox[0].Content.AsVerificationAccept()
			}
			assert.Equal(t, txnID, acceptEvt.TransactionID)
			assert.Equal(t, acceptEvt.Hash, event.VerificationHashMethodSHA256)
			assert.Equal(t, acceptEvt.KeyAgreementProtocol, event.KeyAgreementProtocolCurve25519HKDFSHA256)
			assert.Equal(t, acceptEvt.MessageAuthenticationCode, event.MACMethodHKDFHMACSHA256V2)
			assert.Contains(t, acceptEvt.ShortAuthenticationString, event.SASMethodDecimal)
			assert.Contains(t, acceptEvt.ShortAuthenticationString, event.SASMethodEmoji)
			assert.NotEmpty(t, acceptEvt.Commitment)

			// Test that the first key event is correct
			var firstKeyEvt *event.VerificationKeyEventContent
			if tc.sendingStartsSAS {
				// Process the verification accept event on the sending device.
				ts.dispatchToDevice(t, ctx, sendingClient)

				// Sending device sends first key event to the receiving
				// device.
				receivingInbox := ts.DeviceInbox[aliceUserID][receivingDeviceID]
				assert.Len(t, receivingInbox, 1)
				firstKeyEvt = receivingInbox[0].Content.AsVerificationKey()
			} else {
				// Process the verification accept event on the receiving
				// device.
				ts.dispatchToDevice(t, ctx, receivingClient)

				// Receiving device sends first key event to the sending
				// device.
				sendingInbox := ts.DeviceInbox[aliceUserID][sendingDeviceID]
				assert.Len(t, sendingInbox, 1)
				firstKeyEvt = sendingInbox[0].Content.AsVerificationKey()
			}
			assert.Equal(t, txnID, firstKeyEvt.TransactionID)
			assert.NotEmpty(t, firstKeyEvt.Key)
			assert.Len(t, firstKeyEvt.Key, 32)

			// Test that the second key event is correct
			var secondKeyEvt *event.VerificationKeyEventContent
			if tc.sendingStartsSAS {
				// Process the first key event on the receiving device.
				ts.dispatchToDevice(t, ctx, receivingClient)

				// Receiving device sends second key event to the sending
				// device.
				sendingInbox := ts.DeviceInbox[aliceUserID][sendingDeviceID]
				assert.Len(t, sendingInbox, 1)
				secondKeyEvt = sendingInbox[0].Content.AsVerificationKey()

				// Ensure that the receiving device showed emojis and SAS numbers.
				assert.Len(t, receivingCallbacks.GetDecimalsShown(txnID), 3)
				assert.Len(t, receivingCallbacks.GetEmojisShown(txnID), 7)
			} else {
				// Process the first key event on the sending device.
				ts.dispatchToDevice(t, ctx, sendingClient)

				// Sending device sends second key event to the receiving
				// device.
				receivingInbox := ts.DeviceInbox[aliceUserID][receivingDeviceID]
				assert.Len(t, receivingInbox, 1)
				secondKeyEvt = receivingInbox[0].Content.AsVerificationKey()

				// Ensure that the sending device showed emojis and SAS numbers.
				assert.Len(t, sendingCallbacks.GetDecimalsShown(txnID), 3)
				assert.Len(t, sendingCallbacks.GetEmojisShown(txnID), 7)
			}
			assert.Equal(t, txnID, secondKeyEvt.TransactionID)
			assert.NotEmpty(t, secondKeyEvt.Key)
			assert.Len(t, secondKeyEvt.Key, 32)

			// Ensure that the SAS codes are the same.
			if tc.sendingStartsSAS {
				// Process the second key event on the sending device.
				ts.dispatchToDevice(t, ctx, sendingClient)
			} else {
				// Process the second key event on the receiving device.
				ts.dispatchToDevice(t, ctx, receivingClient)
			}
			assert.Equal(t, sendingCallbacks.GetDecimalsShown(txnID), receivingCallbacks.GetDecimalsShown(txnID))
			assert.Equal(t, sendingCallbacks.GetEmojisShown(txnID), receivingCallbacks.GetEmojisShown(txnID))

			// Test that the first MAC event is correct
			var firstMACEvt *event.VerificationMACEventContent
			if tc.sendingConfirmsFirst {
				err = sendingHelper.ConfirmSAS(ctx, txnID)
				require.NoError(t, err)

				// The receiving device should have received the MAC event.
				receivingInbox := ts.DeviceInbox[aliceUserID][receivingDeviceID]
				assert.Len(t, receivingInbox, 1)
				firstMACEvt = receivingInbox[0].Content.AsVerificationMAC()

				// The MAC event should have a MAC for the sending device ID.
				assert.Contains(t, maps.Keys(firstMACEvt.MAC), id.NewKeyID(id.KeyAlgorithmEd25519, sendingDeviceID.String()))
			} else {
				err = receivingHelper.ConfirmSAS(ctx, txnID)
				require.NoError(t, err)

				// The sending device should have received the MAC event.
				sendingInbox := ts.DeviceInbox[aliceUserID][sendingDeviceID]
				assert.Len(t, sendingInbox, 1)
				firstMACEvt = sendingInbox[0].Content.AsVerificationMAC()

				// The MAC event should have a MAC for the receiving device ID.
				assert.Contains(t, maps.Keys(firstMACEvt.MAC), id.NewKeyID(id.KeyAlgorithmEd25519, receivingDeviceID.String()))
			}
			assert.Equal(t, txnID, firstMACEvt.TransactionID)

			// The master key and the sending device ID should be in the
			// MAC event's mac keys.
			if tc.sendingGeneratedCrossSigningKeys {
				assert.Contains(t, maps.Keys(firstMACEvt.MAC), id.NewKeyID(id.KeyAlgorithmEd25519, sendingCrossSigningKeysCache.MasterKey.PublicKey().String()))
			} else {
				assert.Contains(t, maps.Keys(firstMACEvt.MAC), id.NewKeyID(id.KeyAlgorithmEd25519, receivingCrossSigningKeysCache.MasterKey.PublicKey().String()))
			}

			// Test that the second MAC event is correct
			var secondMACEvt *event.VerificationMACEventContent
			if tc.sendingConfirmsFirst {
				err = receivingHelper.ConfirmSAS(ctx, txnID)
				require.NoError(t, err)

				// The sending device should have received the MAC event.
				sendingInbox := ts.DeviceInbox[aliceUserID][sendingDeviceID]
				assert.Len(t, sendingInbox, 1)
				secondMACEvt = sendingInbox[0].Content.AsVerificationMAC()

				// The MAC event should have a MAC for the receiving device ID.
				assert.Contains(t, maps.Keys(secondMACEvt.MAC), id.NewKeyID(id.KeyAlgorithmEd25519, receivingDeviceID.String()))
			} else {
				err = sendingHelper.ConfirmSAS(ctx, txnID)
				require.NoError(t, err)

				// The receiving device should have received the MAC event.
				receivingInbox := ts.DeviceInbox[aliceUserID][receivingDeviceID]
				assert.Len(t, receivingInbox, 1)
				secondMACEvt = receivingInbox[0].Content.AsVerificationMAC()

				// The MAC event should have a MAC for the sending device ID.
				assert.Contains(t, maps.Keys(secondMACEvt.MAC), id.NewKeyID(id.KeyAlgorithmEd25519, sendingDeviceID.String()))
			}
			assert.Equal(t, txnID, secondMACEvt.TransactionID)

			// The master key and the sending device ID should be in the
			// MAC event's mac keys.
			if tc.sendingGeneratedCrossSigningKeys {
				assert.Contains(t, maps.Keys(firstMACEvt.MAC), id.NewKeyID(id.KeyAlgorithmEd25519, sendingCrossSigningKeysCache.MasterKey.PublicKey().String()))
			} else {
				assert.Contains(t, maps.Keys(firstMACEvt.MAC), id.NewKeyID(id.KeyAlgorithmEd25519, receivingCrossSigningKeysCache.MasterKey.PublicKey().String()))
			}

			// Test the transaction is done on both sides. We have to dispatch
			// twice to process and drain all of the events.
			ts.dispatchToDevice(t, ctx, sendingClient)
			ts.dispatchToDevice(t, ctx, receivingClient)
			ts.dispatchToDevice(t, ctx, sendingClient)
			ts.dispatchToDevice(t, ctx, receivingClient)
			assert.True(t, sendingCallbacks.IsVerificationDone(txnID))
			assert.True(t, receivingCallbacks.IsVerificationDone(txnID))
		})
	}
}
