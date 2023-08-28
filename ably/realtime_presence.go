package ably

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type syncState uint8

const (
	syncInitial syncState = iota + 1
	syncInProgress
	syncComplete
)

// RealtimePresence represents a single presence map of a particular channel.
// It allows entering, leaving and updating presence state for the current client or on behalf of other client.
// It enables the presence set to be entered and subscribed to, and the historic presence set to be retrieved for a channel.
type RealtimePresence struct {
	mtx                 sync.Mutex
	data                interface{}
	serial              string
	messageEmitter      *eventEmitter
	channel             *RealtimeChannel
	members             map[string]*PresenceMessage
	internalMembers     map[string]*PresenceMessage // RTP17
	syncResidualMembers map[string]*PresenceMessage
	state               PresenceAction
	syncMtx             sync.Mutex
	syncState           syncState
}

func newRealtimePresence(channel *RealtimeChannel) *RealtimePresence {
	pres := &RealtimePresence{
		messageEmitter:  newEventEmitter(channel.log()),
		channel:         channel,
		members:         make(map[string]*PresenceMessage),
		internalMembers: make(map[string]*PresenceMessage),
		syncState:       syncInitial,
	}
	// Lock syncMtx to make all callers to Get(true) wait until the presence
	// is in initial sync state. This is to not make them early return
	// with an empty presence list before channel attaches.
	pres.syncMtx.Lock()
	return pres
}

// RTP16c
func (pres *RealtimePresence) verifyChanState() error {
	switch state := pres.channel.State(); state {
	case ChannelStateDetaching, ChannelStateDetached, ChannelStateFailed, ChannelStateSuspended:
		return newError(91001, fmt.Errorf("unable to enter presence channel (invalid channel state: %s)", state.String()))
	default:
		return nil
	}
}

// RTP5a
func (pres *RealtimePresence) onChannelDetachedOrFailed(err error) {
	for k := range pres.members {
		delete(pres.members, k)
	}
	for k := range pres.internalMembers {
		delete(pres.internalMembers, k)
	}
	pres.channel.queue.Fail(err, true)
}

// RTP5f
func (pres *RealtimePresence) onChannelSuspended(err error) {
	pres.channel.queue.Fail(err, true)
}

func (pres *RealtimePresence) send(msg *PresenceMessage) (result, error) {
	// RTP16c
	if err := pres.verifyChanState(); err != nil {
		return nil, err
	}
	presenceSendFunc := func(ctx context.Context) error {
		protomsg := &protocolMessage{
			Action:   actionPresence,
			Channel:  pres.channel.Name,
			Presence: []*PresenceMessage{msg},
		}
		listen := make(chan error, 1)
		onAck := func(err error) {
			listen <- err
		}
		if err := pres.channel.send(protomsg, onAck); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-listen:
			return err
		}
	}
	if pres.channel.state == ChannelStateInitialized {
		_, err := pres.channel.attach()
		if err != nil {
			return nil, err
		}
	}
	return resultFunc(presenceSendFunc), nil
}

func (pres *RealtimePresence) enqueuePresenceMsg() {

}

func (pres *RealtimePresence) syncWait() {
	// If there's an undergoing sync operation or we wait till channel gets
	// attached, the following lock is going to block until the operations
	// complete.
	pres.syncMtx.Lock()
	pres.syncMtx.Unlock()
}

func syncSerial(msg *protocolMessage) string { // RTP18
	if i := strings.IndexRune(msg.ChannelSerial, ':'); i != -1 {
		return msg.ChannelSerial[i+1:]
	}
	return ""
}

func (pres *RealtimePresence) onAttach(msg *protocolMessage, isNewAttach bool) {
	serial := syncSerial(msg)
	pres.mtx.Lock()
	defer pres.mtx.Unlock()
	// TODO - need to move this logic only when channel enters attached state
	if isNewAttach { // RTP17f
		for _, member := range pres.internalMembers {
			err := pres.enterClient(context.Background(), member.ClientID, member.Data, member.ID) // RTP17g
			// RTP17e
			if err != nil {
				pres.log().Errorf("Error for internal member presence enter with id %v, clientId %v, err %v", member.ID, member.ClientID, err)
				change := ChannelStateChange{
					Current:  pres.channel.state,
					Previous: pres.channel.state,
					Reason:   newError(91004, err),
					Resumed:  true,
					Event:    ChannelEventUpdate,
				}
				pres.channel.emitter.Emit(change.Event, change)
			}
		}
	}
	if msg.Flags.Has(flagHasPresence) {
		pres.syncStart(serial)
	} else {
		pres.leaveMembers(pres.members) // RTP19a
		if pres.syncState == syncInitial {
			pres.syncState = syncComplete
			pres.syncMtx.Unlock()
		}
	}
}

// SyncComplete gives true if the initial SYNC operation has completed for the members present on the channel.
func (pres *RealtimePresence) SyncComplete() bool {
	pres.mtx.Lock()
	defer pres.mtx.Unlock()
	return pres.syncState == syncComplete
}

func (pres *RealtimePresence) syncStart(serial string) {
	if pres.syncState == syncInProgress {
		return
	} else if pres.syncState != syncInitial {
		// Sync has started, make all callers to Get(true) wait. If it's channel's
		// initial sync, the callers are already waiting.
		pres.syncMtx.Lock()
	}
	pres.serial = serial
	pres.syncState = syncInProgress
	pres.syncResidualMembers = make(map[string]*PresenceMessage, len(pres.members)) // RTP19
	for memberKey, member := range pres.members {
		pres.syncResidualMembers[memberKey] = member
	}
}

// RTP19, RTP19a
func (pres *RealtimePresence) leaveMembers(members map[string]*PresenceMessage) {
	for memberKey := range members { // RTP19
		delete(pres.members, memberKey)
	}
	for _, msg := range members {
		msg.Action = PresenceActionLeave
		msg.ID = ""
		msg.Timestamp = time.Now().UnixMilli()
		pres.messageEmitter.Emit(msg.Action, (*subscriptionPresenceMessage)(msg)) // RTP2g
	}
}

func (pres *RealtimePresence) syncEnd() {
	if pres.syncState != syncInProgress {
		return
	}
	pres.leaveMembers(pres.syncResidualMembers) // RTP19

	for memberKey, presence := range pres.members { // RTP2f
		if presence.Action == PresenceActionAbsent {
			delete(pres.members, memberKey)
		}
	}
	pres.syncResidualMembers = nil
	pres.syncState = syncComplete
	// Sync has completed, unblock all callers to Get(true) waiting
	// for the sync.
	pres.syncMtx.Unlock()
}

func (pres *RealtimePresence) addPresenceMember(memberMap map[string]*PresenceMessage, memberKey string, presenceMember *PresenceMessage) bool {
	if existingMember, ok := memberMap[memberKey]; ok { // RTP2a
		isMemberNew, err := presenceMember.IsNewerThan(existingMember) // RTP2b
		if err != nil {
			pres.log().Error(err)
			errorInfo := newError(0, err)
			pres.channel.errorEmitter.Emit(subscriptionName("error"), (*errorMessage)(errorInfo))
		}
		if isMemberNew {
			memberMap[memberKey] = presenceMember
			return true
		}
		return false
	}
	memberMap[memberKey] = presenceMember
	return true
}

func (pres *RealtimePresence) removePresenceMember(memberMap map[string]*PresenceMessage, memberKey string, presenceMember *PresenceMessage) bool {
	if existingMember, ok := memberMap[memberKey]; ok { // RTP2a
		isMemberNew, err := presenceMember.IsNewerThan(existingMember) // RTP2b
		if err != nil {
			pres.log().Error(err)
			errorInfo := newError(0, err)
			pres.channel.errorEmitter.Emit(subscriptionName("error"), (*errorMessage)(errorInfo))
		}
		if isMemberNew {
			delete(memberMap, memberKey)
			return existingMember.Action != PresenceActionAbsent
		}
	}
	return false
}

func (pres *RealtimePresence) processIncomingMessage(msg *protocolMessage, syncSerial string) {
	for _, presmsg := range msg.Presence {
		if presmsg.Timestamp == 0 {
			presmsg.Timestamp = msg.Timestamp
		}
	}
	pres.mtx.Lock()
	if syncSerial != "" { //RTP2g
		pres.syncStart(syncSerial)
	}

	// RTP17 - Update internal presence map
	for _, presenceMember := range msg.Presence {
		memberKey := presenceMember.ClientID // RTP17h
		if pres.channel.client.Connection.id != presenceMember.ConnectionID {
			continue
		}
		switch presenceMember.Action {
		case PresenceActionEnter, PresenceActionUpdate, PresenceActionPresent: // RTP2d, RTP17b
			presenceMemberShallowCopy := presenceMember // RTP2g shouldn't mutate action for next loop
			presenceMemberShallowCopy.Action = PresenceActionPresent
			pres.addPresenceMember(pres.internalMembers, memberKey, presenceMemberShallowCopy)
		case PresenceActionLeave: // RTP17b, RTP2e
			if !presenceMember.isServerSynthesized() {
				pres.removePresenceMember(pres.internalMembers, memberKey, presenceMember)
			}
		}
	}

	// Update presence map / channel's member state.
	updatedPresenceMessages := make([]*PresenceMessage, 0, len(msg.Presence))
	for _, presenceMember := range msg.Presence {
		memberKey := presenceMember.ConnectionID + presenceMember.ClientID
		memberUpdated := false
		switch presenceMember.Action {
		case PresenceActionEnter, PresenceActionUpdate, PresenceActionPresent: // RTP2d
			delete(pres.syncResidualMembers, memberKey)
			presenceMemberShallowCopy := presenceMember
			presenceMemberShallowCopy.Action = PresenceActionPresent // RTP2g
			memberUpdated = pres.addPresenceMember(pres.members, memberKey, presenceMemberShallowCopy)
		case PresenceActionLeave: // RTP2e
			memberUpdated = pres.removePresenceMember(pres.members, memberKey, presenceMember)
		}
		if memberUpdated {
			updatedPresenceMessages = append(updatedPresenceMessages, presenceMember)
		}
	}

	if syncSerial == "" {
		pres.syncEnd()
	}
	pres.mtx.Unlock()
	msg.Count = len(updatedPresenceMessages)
	msg.Presence = updatedPresenceMessages
	for _, msg := range msg.Presence {
		pres.messageEmitter.Emit(msg.Action, (*subscriptionPresenceMessage)(msg)) // RTP2g
	}
}

// Get retrieves the current members (array of [ably.PresenceMessage] objects) present on the channel
// and the metadata for each member, such as their [ably.PresenceAction] and ID (RTP11).
// If the channel state is initialised or non-attached, it will be updated to [ably.ChannelStateAttached].
//
// If the context is cancelled before the operation finishes, the call
// returns with an error, but the operation carries on in the background and
// the channel may eventually be attached anyway (RTP11).
func (pres *RealtimePresence) Get(ctx context.Context) ([]*PresenceMessage, error) {
	return pres.GetWithOptions(ctx)
}

// A PresenceGetOption is an optional parameter for RealtimePresence.GetWithOptions.
type PresenceGetOption func(*presenceGetOptions)

// PresenceGetWithWaitForSync sets whether to wait for a full presence set synchronization between Ably
// and the clients on the channel to complete before returning the results.
// Synchronization begins as soon as the channel is [ably.ChannelStateAttached].
// When set to true the results will be returned as soon as the sync is complete.
// When set to false the current list of members will be returned without the sync completing.
// The default is true (RTP11c1).
func PresenceGetWithWaitForSync(wait bool) PresenceGetOption {
	return func(o *presenceGetOptions) {
		o.waitForSync = true
	}
}

type presenceGetOptions struct {
	waitForSync bool
}

func (o *presenceGetOptions) applyWithDefaults(options ...PresenceGetOption) {
	o.waitForSync = true
	for _, opt := range options {
		opt(o)
	}
}

// GetWithOptions is Get with optional parameters.
// Retrieves the current members (array of [ably.PresenceMessage] objects) present on the channel
// and the metadata for each member, such as their [ably.PresenceAction] and ID (RTP11).
// If the channel state is initialised or non-attached, it will be updated to [ably.ChannelStateAttached].
//
// If the context is cancelled before the operation finishes, the call
// returns with an error, but the operation carries on in the background and
// the channel may eventually be attached anyway (RTP11).
func (pres *RealtimePresence) GetWithOptions(ctx context.Context, options ...PresenceGetOption) ([]*PresenceMessage, error) {
	var opts presenceGetOptions
	opts.applyWithDefaults(options...)

	res, err := pres.channel.attach()
	if err != nil {
		return nil, err
	}
	// TODO: Don't ignore context.
	err = res.Wait(ctx)
	if err != nil {
		return nil, err
	}

	if opts.waitForSync {
		pres.syncWait()
	}

	pres.mtx.Lock()
	defer pres.mtx.Unlock()
	members := make([]*PresenceMessage, 0, len(pres.members))
	for _, member := range pres.members {
		members = append(members, member)
	}
	return members, nil
}

type subscriptionPresenceMessage PresenceMessage

func (*subscriptionPresenceMessage) isEmitterData() {}

// Subscribe registers a event listener that is called each time a received [ably.PresenceMessage] matches given
// [ably.PresenceAction] or an action within an array of [ably.PresenceAction], such as a new member entering
// the presence set. A callback may optionally be passed in to this call to be notified of success or failure of
// the channel RealtimeChannel.Attach operation (RTP6b).
//
// This implicitly attaches the channel if it's not already attached. If the
// context is cancelled before the attach operation finishes, the call
// returns with an error, but the operation carries on in the background and
// the channel may eventually be attached anyway.
//
// See package-level documentation => [ably] Event Emitters for details about messages dispatch.
func (pres *RealtimePresence) Subscribe(ctx context.Context, action PresenceAction, handle func(*PresenceMessage)) (func(), error) {
	// unsubscribe deregisters a specific listener that is registered to receive [ably.PresenceMessage]
	// on the channel for a given [ably.PresenceAction] (RTP7b).
	unsubscribe := pres.messageEmitter.On(action, func(message emitterData) {
		handle((*PresenceMessage)(message.(*subscriptionPresenceMessage)))
	})
	res, err := pres.channel.attach()
	if err != nil {
		unsubscribe()
		return nil, err
	}
	err = res.Wait(ctx)
	if err != nil {
		unsubscribe()
		return nil, err
	}
	return unsubscribe, err
}

// SubscribeAll registers a event listener that is called each time a received [ably.PresenceMessage] such as a
// new member entering the presence set. A callback may optionally be passed in to this call to be notified of
// success or failure of the channel RealtimeChannel.Attach operation (RTP6a).
//
// This implicitly attaches the channel if it's not already attached. If the
// context is cancelled before the attach operation finishes, the call
// returns with an error, but the operation carries on in the background and
// the channel may eventually be attached anyway.
//
// See package-level documentation => [ably] Event Emitters for details about messages dispatch.
func (pres *RealtimePresence) SubscribeAll(ctx context.Context, handle func(*PresenceMessage)) (func(), error) {
	// unsubscribe Deregisters a specific listener that is registered
	// to receive [ably.PresenceMessage] on the channel (RTP7a).
	unsubscribe := pres.messageEmitter.OnAll(func(message emitterData) {
		handle((*PresenceMessage)(message.(*subscriptionPresenceMessage)))
	})
	res, err := pres.channel.attach()
	if err != nil {
		unsubscribe()
		return nil, err
	}
	err = res.Wait(ctx)
	if err != nil {
		unsubscribe()
		return nil, err
	}
	return unsubscribe, nil
}

// Enter announces the presence of the current client with optional data payload (enter message) on the channel.
// It enters client presence into the channel presence set.
// A clientId is required to be present on a channel (RTP8).
// If this connection has no clientID then this function will fail.
//
// If the context is cancelled before the operation finishes, the call
// returns with an error, but the operation carries on in the background and
// presence state may eventually be updated anyway.
func (pres *RealtimePresence) Enter(ctx context.Context, data interface{}) error {
	clientID := pres.auth().ClientID()
	if clientID == "" {
		return newError(91000, nil)
	}
	return pres.EnterClient(ctx, clientID, data)
}

// Update announces an updated presence message for the current client. Updates the data payload for a presence member.
// If the current client is not present on the channel, Update will behave as Enter method,
// i.e. if called before entering the presence set, this is treated as an [ably.PresenceActionEnter] event (RTP9).
// If this connection has no clientID then this function will fail.
//
// If the context is cancelled before the operation finishes, the call
// returns with an error, but the operation carries on in the background and
// presence state may eventually be updated anyway.
func (pres *RealtimePresence) Update(ctx context.Context, data interface{}) error {
	clientID := pres.auth().ClientID()
	if clientID == "" {
		return newError(91000, nil)
	}
	return pres.UpdateClient(ctx, clientID, data)
}

// Leave announces current client leave on the channel altogether with an optional data payload (leave message).
// It is removed from the channel presence members set. A client must have previously entered the presence set
// before they can leave it (RTP10).
//
// If the context is cancelled before the operation finishes, the call returns with an error,
// but the operation carries on in the background and presence state may eventually be updated anyway.
func (pres *RealtimePresence) Leave(ctx context.Context, data interface{}) error {
	clientID := pres.auth().ClientID()
	if clientID == "" {
		return newError(91000, nil)
	}
	return pres.LeaveClient(ctx, clientID, data)
}

// EnterClient announces presence of the given clientID altogether with a enter message for the associated channel.
// It enters given clientId in the channel presence set. It enables a single client to update presence on
// behalf of any number of clients using a single connection. The library must have been instantiated with an API key
// or a token bound to a wildcard (*) clientId (RTP4, RTP14, RTP15).
//
// If the context is cancelled before the operation finishes, the call returns with an error,
// but the operation carries on in the background and presence state may eventually be updated anyway.
func (pres *RealtimePresence) EnterClient(ctx context.Context, clientID string, data interface{}) error {
	return pres.enterClient(ctx, clientID, data, "")
}

func (pres *RealtimePresence) enterClient(ctx context.Context, clientID string, data interface{}, msgId string) error {
	pres.mtx.Lock()
	pres.data = data
	pres.state = PresenceActionEnter
	pres.mtx.Unlock()
	msg := PresenceMessage{
		Action: PresenceActionEnter,
	}
	msg.Data = data
	msg.ClientID = clientID
	msg.ID = msgId
	res, err := pres.send(&msg)
	if err != nil {
		return err
	}
	return res.Wait(ctx)
}

func nonnil(a, b interface{}) interface{} {
	if a != nil {
		return a
	}
	return b
}

// UpdateClient announces an updated presence message for the given clientID.
// If the given clientID is not present on the channel, Update will behave as Enter method.
// Updates the data payload for a presence member using a given clientId.
// Enables a single client to update presence on behalf of any number of clients using a single connection.
// The library must have been instantiated with an API key or a token bound to a wildcard (*) clientId (RTP15).
//
// If the context is cancelled before the operation finishes, the call returns with an error,
// but the operation carries on in the background and presence data may eventually be updated anyway.
func (pres *RealtimePresence) UpdateClient(ctx context.Context, clientID string, data interface{}) error {
	pres.mtx.Lock()
	if pres.state != PresenceActionEnter {
		oldData := pres.data
		pres.mtx.Unlock()
		return pres.EnterClient(ctx, clientID, nonnil(data, oldData))
	}
	pres.data = data
	pres.mtx.Unlock()
	msg := PresenceMessage{
		Action: PresenceActionUpdate,
	}
	msg.ClientID = clientID
	msg.Data = data
	res, err := pres.send(&msg)
	if err != nil {
		return err
	}
	return res.Wait(ctx)
}

// LeaveClient announces the given clientID leave from the associated channel altogether with a optional leave message.
// Leaves the given clientId from channel presence set. Enables a single client to update presence
// on behalf of any number of clients using a single connection. The library must have been instantiated with
// an API key or a token bound to a wildcard (*) clientId (RTP15).
//
// If the context is cancelled before the operation finishes, the call returns with an error,
// but the operation carries on in the background and presence data may eventually be updated anyway.
func (pres *RealtimePresence) LeaveClient(ctx context.Context, clientID string, data interface{}) error {
	pres.mtx.Lock()
	if pres.data == nil {
		pres.data = data
	}
	pres.mtx.Unlock()

	msg := PresenceMessage{
		Action: PresenceActionLeave,
	}
	msg.ClientID = clientID
	msg.Data = data
	res, err := pres.send(&msg)
	if err != nil {
		return err
	}
	return res.Wait(ctx)
}

func (pres *RealtimePresence) auth() *Auth {
	return pres.channel.client.Auth
}

func (pres *RealtimePresence) log() logger {
	return pres.channel.log()
}
