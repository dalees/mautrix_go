// Copyright (c) 2024 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func (portal *Portal) doForwardBackfill(ctx context.Context, source *UserLogin, lastMessage *database.Message) {
	log := zerolog.Ctx(ctx).With().Str("action", "forward backfill").Logger()
	ctx = log.WithContext(ctx)
	api, ok := source.Client.(BackfillingNetworkAPI)
	if !ok {
		log.Debug().Msg("Network API does not support backfilling")
		return
	}
	log.Info().Str("latest_message_id", string(lastMessage.ID)).Msg("Fetching messages for forward backfill")
	resp, err := api.FetchMessages(ctx, FetchMessagesParams{
		Portal:        portal,
		ThreadRoot:    "",
		Forward:       true,
		AnchorMessage: lastMessage,
		Count:         100,
	})
	if err != nil {
		log.Err(err).Msg("Failed to fetch messages for forward backfill")
		return
	}
	portal.sendBackfill(ctx, source, resp.Messages, true, resp.MarkRead, lastMessage)
}

func (portal *Portal) DoBackwardsBackfill(ctx context.Context, source *UserLogin) {
	//log := zerolog.Ctx(ctx)
	//api, ok := source.Client.(BackfillingNetworkAPI)
	//if !ok {
	//	log.Debug().Msg("Network API does not support backfilling")
	//	return
	//}
	//resp, err := api.FetchMessages(ctx, FetchMessagesParams{
	//	Portal:        portal,
	//	ThreadRoot:    "",
	//	Forward:       true,
	//	AnchorMessage: lastMessage,
	//	Count:         100,
	//})
	//if err != nil {
	//	log.Err(err).Msg("Failed to fetch messages for forward backfill")
	//	return
	//}
	//portal.sendBackfill(ctx, source, resp.Messages, false, resp.MarkRead, lastMessage)
}

func (portal *Portal) sendBackfill(ctx context.Context, source *UserLogin, messages []*BackfillMessage, forceForward, markRead bool, lastMessage *database.Message) {
	if forceForward {
		var cutoff int
		for i, msg := range messages {
			if msg.Timestamp.Before(lastMessage.Timestamp) {
				cutoff = i
			}
		}
		if cutoff != 0 {
			zerolog.Ctx(ctx).Debug().
				Int("cutoff_count", cutoff).
				Int("total_count", len(messages)).
				Time("last_bridged_ts", lastMessage.Timestamp).
				Msg("Cutting off forward backfill messages older than latest bridged message")
			messages = messages[cutoff:]
		}
	}
	canBatchSend := portal.Bridge.Matrix.GetCapabilities().BatchSending
	zerolog.Ctx(ctx).Info().Int("message_count", len(messages)).Bool("batch_send", canBatchSend).Msg("Sending backfill messages")
	if canBatchSend {
		portal.sendBatch(ctx, source, messages, forceForward, markRead)
	} else {
		portal.sendLegacyBackfill(ctx, source, messages, markRead)
	}
	zerolog.Ctx(ctx).Debug().Msg("Backfill finished")
}

func (portal *Portal) sendBatch(ctx context.Context, source *UserLogin, messages []*BackfillMessage, forceForward, markRead bool) {
	req := &mautrix.ReqBeeperBatchSend{
		ForwardIfNoMessages: !forceForward,
		Forward:             forceForward,
		Events:              make([]*event.Event, 0, len(messages)),
	}
	if markRead {
		req.MarkReadBy = source.UserMXID
	} else {
		req.SendNotification = forceForward
	}
	prevThreadEvents := make(map[networkid.MessageID]id.EventID)
	dbMessages := make([]*database.Message, 0, len(messages))
	var disappearingMessages []*database.DisappearingMessage
	for _, msg := range messages {
		intent := portal.GetIntentFor(ctx, msg.Sender, source, RemoteEventMessage)
		replyTo, threadRoot, prevThreadEvent := portal.getRelationMeta(ctx, msg.ReplyTo, msg.ThreadRoot, true)
		if threadRoot != nil && prevThreadEvents[*msg.ThreadRoot] != "" {
			prevThreadEvent.MXID = prevThreadEvents[*msg.ThreadRoot]
		}
		for _, part := range msg.Parts {
			portal.applyRelationMeta(part.Content, replyTo, threadRoot, prevThreadEvent)
			evtID := portal.Bridge.Matrix.GenerateDeterministicEventID(portal.MXID, portal.PortalKey, msg.ID, part.ID)
			req.Events = append(req.Events, &event.Event{
				Sender:    intent.GetMXID(),
				Type:      part.Type,
				Timestamp: msg.Timestamp.UnixMilli(),
				ID:        evtID,
				RoomID:    portal.MXID,
				Content: event.Content{
					Parsed: part.Content,
					Raw:    part.Extra,
				},
			})
			dbMessages = append(dbMessages, &database.Message{
				ID:         msg.ID,
				PartID:     part.ID,
				MXID:       evtID,
				Room:       portal.PortalKey,
				SenderID:   msg.Sender.Sender,
				Timestamp:  msg.Timestamp,
				ThreadRoot: ptr.Val(msg.ThreadRoot),
				ReplyTo:    ptr.Val(msg.ReplyTo),
				Metadata: database.MessageMetadata{
					StandardMessageMetadata: database.StandardMessageMetadata{
						SenderMXID: intent.GetMXID(),
					},
					Extra: part.DBMetadata,
				},
			})
			if prevThreadEvent != nil {
				prevThreadEvent.MXID = evtID
				prevThreadEvents[*msg.ThreadRoot] = evtID
			}
			if msg.Disappear.Type != database.DisappearingTypeNone {
				if msg.Disappear.Type == database.DisappearingTypeAfterSend && msg.Disappear.DisappearAt.IsZero() {
					msg.Disappear.DisappearAt = msg.Timestamp.Add(msg.Disappear.Timer)
				}
				disappearingMessages = append(disappearingMessages, &database.DisappearingMessage{
					RoomID:              portal.MXID,
					EventID:             evtID,
					DisappearingSetting: msg.Disappear,
				})
			}
		}
		// TODO handle reactions
		//for _, reaction := range msg.Reactions {
		//}
	}
	_, err := portal.Bridge.Matrix.BatchSend(ctx, portal.MXID, req)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to send backfill messages")
	}
	if len(disappearingMessages) > 0 {
		go func() {
			for _, msg := range disappearingMessages {
				portal.Bridge.DisappearLoop.Add(ctx, msg)
			}
		}()
	}
	for _, msg := range dbMessages {
		err := portal.Bridge.DB.Message.Insert(ctx, msg)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).
				Str("message_id", string(msg.ID)).
				Str("part_id", string(msg.PartID)).
				Msg("Failed to insert backfilled message to database")
		}
	}
}

func (portal *Portal) sendLegacyBackfill(ctx context.Context, source *UserLogin, messages []*BackfillMessage, markRead bool) {
	var lastPart id.EventID
	for _, msg := range messages {
		intent := portal.GetIntentFor(ctx, msg.Sender, source, RemoteEventMessage)
		dbMessages := portal.sendConvertedMessage(ctx, msg.ID, intent, msg.Sender, msg.ConvertedMessage, msg.Timestamp, func(z *zerolog.Event) *zerolog.Event {
			return z.
				Str("message_id", string(msg.ID)).
				Any("sender_id", msg.Sender).
				Time("message_ts", msg.Timestamp)
		})
		if len(dbMessages) > 0 {
			lastPart = dbMessages[len(dbMessages)-1].MXID
		}
		// TODO handle reactions
		//for _, reaction := range msg.Reactions {
		//}
	}
	if markRead {
		dp := source.User.DoublePuppet(ctx)
		if dp != nil {
			err := dp.MarkRead(ctx, portal.MXID, lastPart, time.Now())
			if err != nil {
				zerolog.Ctx(ctx).Err(err).Msg("Failed to mark room as read after backfill")
			}
		}
	}
}
