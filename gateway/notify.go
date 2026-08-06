package main

// wsBalanceChangedMsg, wsTransferUpdatedMsg, and wsDepositUpdatedMsg are
// the only shapes ever pushed over a WS connection in response to a Kafka
// event: a signal, not data. Delivery isn't ordered or guaranteed — an
// event can arrive after a later one, or not at all if this instance
// wasn't holding the connection at the time — so a client can never treat
// a value carried on the wire here as current. It always re-fetches the
// authoritative amount/balance over plain HTTP, where auth and
// consistency already live. That's why none of these carry an amount or
// a balance, even though the underlying proto events do.
type wsBalanceChangedMsg struct {
	Type string `json:"type"`
}

type wsTransferUpdatedMsg struct {
	Type       string `json:"type"`
	TransferID string `json:"transfer_id"`
}

type wsDepositUpdatedMsg struct {
	Type      string `json:"type"`
	DepositID string `json:"deposit_id"`
}

// wsSignal pairs a message with the single user_id it's addressed to.
// There is deliberately no broadcast primitive anywhere in this package —
// every signal this file produces names exactly one recipient, and
// wsRegistry.send only ever delivers to that one user's own connections.
type wsSignal struct {
	userID string
	msg    any
}

// resolveAccountUser maps an account_id to the user_id that owns it,
// reporting false if it can't be resolved (e.g. accounts-svc is
// unreachable and the account isn't cached). Injected rather than called
// directly against accountCache so the routing decisions below can be
// tested without any cache, HTTP, or Kafka involved.
type resolveAccountUser func(accountID string) (userID string, ok bool)

// signalsForTransferEvent decides who learns about a transfer event and
// with what minimal signal. This is the structural enforcement of two
// rules that must never regress:
//
//  1. Never broadcast — every signal names exactly the one user whose own
//     money moved (or didn't).
//  2. Sprint 7: the recipient of a transfer that never moved money learns
//     nothing at all, not even that it was attempted. That's enforced
//     here by never calling resolve(recipientAccountID) at all unless
//     eventType is TransferCompleted — not by filtering out a signal
//     after the fact, so there's no path that accidentally resolves (and
//     could leak, via logs or a future bug) the recipient's identity for
//     a transfer they were never actually party to.
func signalsForTransferEvent(eventType, transferID, senderAccountID, recipientAccountID string, resolve resolveAccountUser) []wsSignal {
	var out []wsSignal

	if senderUserID, ok := resolve(senderAccountID); ok {
		if eventType == eventTypeTransferCompleted {
			out = append(out, wsSignal{senderUserID, wsBalanceChangedMsg{Type: "balance.changed"}})
		}
		out = append(out, wsSignal{senderUserID, wsTransferUpdatedMsg{Type: "transfer.updated", TransferID: transferID}})
	}

	if eventType != eventTypeTransferCompleted {
		return out
	}
	if recipientUserID, ok := resolve(recipientAccountID); ok {
		out = append(out, wsSignal{recipientUserID, wsBalanceChangedMsg{Type: "balance.changed"}})
		out = append(out, wsSignal{recipientUserID, wsTransferUpdatedMsg{Type: "transfer.updated", TransferID: transferID}})
	}
	return out
}

// signalsForDepositEvent is the deposit equivalent — a deposit has only
// one party, so there's no sender/recipient asymmetry to enforce.
func signalsForDepositEvent(depositID, accountID string, resolve resolveAccountUser) []wsSignal {
	userID, ok := resolve(accountID)
	if !ok {
		return nil
	}
	return []wsSignal{
		{userID, wsBalanceChangedMsg{Type: "balance.changed"}},
		{userID, wsDepositUpdatedMsg{Type: "deposit.updated", DepositID: depositID}},
	}
}
