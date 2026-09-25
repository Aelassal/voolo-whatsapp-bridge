// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/logx"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
)

// The codes that may appear in stderr lines (PROTOCOL.md §10.4): protocol
// error codes, states, signal kinds, logout and pairing reasons, command and
// event types. logx writes any other value as "redacted".
func init() {
	logx.AllowCodes(protocol.ErrorCodes()...)
	logx.AllowCodes(protocol.Commands()...)
	logx.AllowCodes(
		protocol.StateUnpaired, protocol.StateConnecting, protocol.StateConnected, protocol.StateReconnecting, protocol.StateStopped,
		protocol.SigRateLimited, protocol.SigTempBanned, protocol.SigStreamReplaced, protocol.SigClientOutdated,
		protocol.SigConnectFailure, protocol.SigStreamError, protocol.SigKeepaliveTimeout,
		protocol.LogoutUser, protocol.LogoutDeviceRemoved, protocol.LogoutPrimaryGone, protocol.LogoutBanned, protocol.LogoutUnknown,
		protocol.PairTimeout, protocol.PairRejected, protocol.PairClientOutdated, protocol.PairError,
		protocol.EvHello, protocol.EvReady, protocol.EvStatus, protocol.EvSignal, protocol.EvQR, protocol.EvPairCode, protocol.EvPaired,
		protocol.EvPairFailed, protocol.EvLoggedOut, protocol.EvSyncProgress, protocol.EvChat, protocol.EvChatUpdate, protocol.EvContact,
		protocol.EvGroup, protocol.EvMessage, protocol.EvMsgUpdate, protocol.EvReaction, protocol.EvReceipt, protocol.EvHistoryBatch,
		protocol.EvMediaReady, protocol.EvSendResult, protocol.EvOK, protocol.EvPong, protocol.EvError,
	)
}
