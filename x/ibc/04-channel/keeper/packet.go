package keeper

import (
	"bytes"
	"fmt"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	capabilitytypes "github.com/cosmos/cosmos-sdk/x/capability/types"
	clienttypes "github.com/cosmos/cosmos-sdk/x/ibc/02-client/types"
	connectiontypes "github.com/cosmos/cosmos-sdk/x/ibc/03-connection/types"
	"github.com/cosmos/cosmos-sdk/x/ibc/04-channel/types"
	host "github.com/cosmos/cosmos-sdk/x/ibc/24-host"
	"github.com/cosmos/cosmos-sdk/x/ibc/exported"
)

// SendPacket is called by a module in order to send an IBC packet on a channel
// end owned by the calling module to the corresponding module on the counterparty
// chain.
func (k Keeper) SendPacket(
	ctx sdk.Context,
	channelCap *capabilitytypes.Capability,
	packet exported.PacketI,
) error {
	if err := packet.ValidateBasic(); err != nil {
		return sdkerrors.Wrap(err, "packet failed basic validation")
	}

	channel, found := k.GetChannel(ctx, packet.GetSourcePort(), packet.GetSourceChannel())
	if !found {
		return sdkerrors.Wrap(types.ErrChannelNotFound, packet.GetSourceChannel())
	}

	if channel.State == types.CLOSED {
		return sdkerrors.Wrapf(
			types.ErrInvalidChannelState,
			"channel is CLOSED (got %s)", channel.State.String(),
		)
	}

	if !k.scopedKeeper.AuthenticateCapability(ctx, channelCap, host.ChannelCapabilityPath(packet.GetSourcePort(), packet.GetSourceChannel())) {
		return sdkerrors.Wrapf(types.ErrChannelCapabilityNotFound, "caller does not own capability for channel, port ID (%s) channel ID (%s)", packet.GetSourcePort(), packet.GetSourceChannel())
	}

	if packet.GetDestPort() != channel.Counterparty.PortId {
		return sdkerrors.Wrapf(
			types.ErrInvalidPacket,
			"packet destination port doesn't match the counterparty's port (%s ≠ %s)", packet.GetDestPort(), channel.Counterparty.PortId,
		)
	}

	if packet.GetDestChannel() != channel.Counterparty.ChannelId {
		return sdkerrors.Wrapf(
			types.ErrInvalidPacket,
			"packet destination channel doesn't match the counterparty's channel (%s ≠ %s)", packet.GetDestChannel(), channel.Counterparty.ChannelId,
		)
	}

	connectionEnd, found := k.connectionKeeper.GetConnection(ctx, channel.ConnectionHops[0])
	if !found {
		return sdkerrors.Wrap(connectiontypes.ErrConnectionNotFound, channel.ConnectionHops[0])
	}

	clientState, found := k.clientKeeper.GetClientState(ctx, connectionEnd.GetClientID())
	if !found {
		return clienttypes.ErrConsensusStateNotFound
	}

	// check if packet timeouted on the receiving chain
	latestHeight := clientState.GetLatestHeight()
	timeoutHeight := packet.GetTimeoutHeight()
	if !timeoutHeight.IsZero() && latestHeight.GTE(timeoutHeight) {
		return sdkerrors.Wrapf(
			types.ErrPacketTimeout,
			"receiving chain block height >= packet timeout height (%s >= %s)", latestHeight, timeoutHeight,
		)
	}

	latestTimestamp, err := k.connectionKeeper.GetTimestampAtHeight(ctx, connectionEnd, latestHeight)
	if err != nil {
		return err
	}

	if packet.GetTimeoutTimestamp() != 0 && latestTimestamp >= packet.GetTimeoutTimestamp() {
		return sdkerrors.Wrapf(
			types.ErrPacketTimeout,
			"receiving chain block timestamp >= packet timeout timestamp (%s >= %s)", time.Unix(0, int64(latestTimestamp)), time.Unix(0, int64(packet.GetTimeoutTimestamp())),
		)
	}

	nextSequenceSend, found := k.GetNextSequenceSend(ctx, packet.GetSourcePort(), packet.GetSourceChannel())
	if !found {
		return sdkerrors.Wrapf(
			types.ErrSequenceSendNotFound,
			"source port: %s, source channel: %s", packet.GetSourcePort(), packet.GetSourceChannel(),
		)
	}

	if packet.GetSequence() != nextSequenceSend {
		return sdkerrors.Wrapf(
			types.ErrInvalidPacket,
			"packet sequence ≠ next send sequence (%d ≠ %d)", packet.GetSequence(), nextSequenceSend,
		)
	}

	nextSequenceSend++
	k.SetNextSequenceSend(ctx, packet.GetSourcePort(), packet.GetSourceChannel(), nextSequenceSend)
	k.SetPacketCommitment(ctx, packet.GetSourcePort(), packet.GetSourceChannel(), packet.GetSequence(), types.CommitPacket(packet))

	// Emit Event with Packet data along with other packet information for relayer to pick up
	// and relay to other chain
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeSendPacket,
			sdk.NewAttribute(types.AttributeKeyData, string(packet.GetData())),
			sdk.NewAttribute(types.AttributeKeyTimeoutHeight, timeoutHeight.String()),
			sdk.NewAttribute(types.AttributeKeyTimeoutTimestamp, fmt.Sprintf("%d", packet.GetTimeoutTimestamp())),
			sdk.NewAttribute(types.AttributeKeySequence, fmt.Sprintf("%d", packet.GetSequence())),
			sdk.NewAttribute(types.AttributeKeySrcPort, packet.GetSourcePort()),
			sdk.NewAttribute(types.AttributeKeySrcChannel, packet.GetSourceChannel()),
			sdk.NewAttribute(types.AttributeKeyDstPort, packet.GetDestPort()),
			sdk.NewAttribute(types.AttributeKeyDstChannel, packet.GetDestChannel()),
			sdk.NewAttribute(types.AttributeKeyChannelOrdering, channel.Ordering.String()),
		),
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
		),
	})

	k.Logger(ctx).Info(fmt.Sprintf("packet sent: %v", packet))
	return nil
}

// RecvPacket is called by a module in order to receive & process an IBC packet
// sent on the corresponding channel end on the counterparty chain.
func (k Keeper) RecvPacket(
	ctx sdk.Context,
	packet exported.PacketI,
	proof []byte,
	proofHeight exported.Height,
) error {
	channel, found := k.GetChannel(ctx, packet.GetDestPort(), packet.GetDestChannel())
	if !found {
		return sdkerrors.Wrap(types.ErrChannelNotFound, packet.GetDestChannel())
	}

	if channel.State != types.OPEN {
		return sdkerrors.Wrapf(
			types.ErrInvalidChannelState,
			"channel state is not OPEN (got %s)", channel.State.String(),
		)
	}

	// NOTE: RecvPacket is called by the AnteHandler which acts upon the packet.Route(),
	// so the capability authentication can be omitted here

	// packet must come from the channel's counterparty
	if packet.GetSourcePort() != channel.Counterparty.PortId {
		return sdkerrors.Wrapf(
			types.ErrInvalidPacket,
			"packet source port doesn't match the counterparty's port (%s ≠ %s)", packet.GetSourcePort(), channel.Counterparty.PortId,
		)
	}

	if packet.GetSourceChannel() != channel.Counterparty.ChannelId {
		return sdkerrors.Wrapf(
			types.ErrInvalidPacket,
			"packet source channel doesn't match the counterparty's channel (%s ≠ %s)", packet.GetSourceChannel(), channel.Counterparty.ChannelId,
		)
	}

	connectionEnd, found := k.connectionKeeper.GetConnection(ctx, channel.ConnectionHops[0])
	if !found {
		return sdkerrors.Wrap(connectiontypes.ErrConnectionNotFound, channel.ConnectionHops[0])
	}

	if connectionEnd.GetState() != int32(connectiontypes.OPEN) {
		return sdkerrors.Wrapf(
			connectiontypes.ErrInvalidConnectionState,
			"connection state is not OPEN (got %s)", connectiontypes.State(connectionEnd.GetState()).String(),
		)
	}

	// check if packet timeouted by comparing it with the latest height of the chain
	selfHeight := clienttypes.GetSelfHeight(ctx)
	timeoutHeight := packet.GetTimeoutHeight()
	if !timeoutHeight.IsZero() && selfHeight.GTE(timeoutHeight) {
		return sdkerrors.Wrapf(
			types.ErrPacketTimeout,
			"block height >= packet timeout height (%s >= %s)", selfHeight, timeoutHeight,
		)
	}

	// check if packet timeouted by comparing it with the latest timestamp of the chain
	if packet.GetTimeoutTimestamp() != 0 && uint64(ctx.BlockTime().UnixNano()) >= packet.GetTimeoutTimestamp() {
		return sdkerrors.Wrapf(
			types.ErrPacketTimeout,
			"block timestamp >= packet timeout timestamp (%s >= %s)", ctx.BlockTime(), time.Unix(0, int64(packet.GetTimeoutTimestamp())),
		)
	}

	switch channel.Ordering {
	case types.UNORDERED:
		// check if the packet receipt has been received already for unordered channels
		_, found := k.GetPacketReceipt(ctx, packet.GetDestPort(), packet.GetDestChannel(), packet.GetSequence())
		if found {
			return sdkerrors.Wrapf(
				types.ErrInvalidPacket,
				"packet sequence (%d) already has been received", packet.GetSequence(),
			)
		}

	case types.ORDERED:
		// check if the packet is being received in order
		nextSequenceRecv, found := k.GetNextSequenceRecv(ctx, packet.GetDestPort(), packet.GetDestChannel())
		if !found {
			return sdkerrors.Wrapf(
				types.ErrSequenceReceiveNotFound,
				"destination port: %s, destination channel: %s", packet.GetDestPort(), packet.GetDestChannel(),
			)
		}

		if packet.GetSequence() != nextSequenceRecv {
			return sdkerrors.Wrapf(
				types.ErrInvalidPacket,
				"packet sequence ≠ next receive sequence (%d ≠ %d)", packet.GetSequence(), nextSequenceRecv,
			)
		}
	}

	if err := k.connectionKeeper.VerifyPacketCommitment(
		ctx, connectionEnd, proofHeight, proof,
		packet.GetSourcePort(), packet.GetSourceChannel(), packet.GetSequence(),
		types.CommitPacket(packet),
	); err != nil {
		return sdkerrors.Wrap(err, "couldn't verify counterparty packet commitment")
	}

	// NOTE: the remaining code is located in the WriteReceipt function
	return nil
}

// WriteReceipt updates the receive sequence in the case of an ordered channel or sets an empty receipt
// if the channel is unordered.
//
// CONTRACT: this function must be called in the IBC handler
func (k Keeper) WriteReceipt(
	ctx sdk.Context,
	chanCap *capabilitytypes.Capability,
	packet exported.PacketI,
) error {
	channel, found := k.GetChannel(ctx, packet.GetDestPort(), packet.GetDestChannel())
	if !found {
		return sdkerrors.Wrapf(types.ErrChannelNotFound, packet.GetDestChannel())
	}

	// sanity check
	if channel.State != types.OPEN {
		return sdkerrors.Wrapf(
			types.ErrInvalidChannelState,
			"channel state is not OPEN (got %s)", channel.State.String(),
		)
	}

	capName := host.ChannelCapabilityPath(packet.GetDestPort(), packet.GetDestChannel())
	if !k.scopedKeeper.AuthenticateCapability(ctx, chanCap, capName) {
		return sdkerrors.Wrapf(
			types.ErrInvalidChannelCapability,
			"channel capability failed authentication for capability name %s", capName,
		)
	}

	switch channel.Ordering {
	case types.ORDERED:
		nextSequenceRecv, found := k.GetNextSequenceRecv(ctx, packet.GetDestPort(), packet.GetDestChannel())
		if !found {
			return sdkerrors.Wrapf(
				types.ErrSequenceReceiveNotFound,
				"destination port: %s, destination channel: %s", packet.GetDestPort(), packet.GetDestChannel(),
			)
		}

		nextSequenceRecv++

		// incrementing nextSequenceRecv and storing under this chain's channelEnd identifiers
		// Since this is the receiving chain, our channelEnd is packet's destination port and channel
		k.SetNextSequenceRecv(ctx, packet.GetDestPort(), packet.GetDestChannel(), nextSequenceRecv)

	case types.UNORDERED:
		// For unordered channels we must set the receipt so it can be verified on the other side.
		// This receipt does not contain any data, since the packet has not yet been processed,
		// it's just a single store key set to an empty string to indicate that the packet has been received
		_, found := k.GetPacketReceipt(ctx, packet.GetDestPort(), packet.GetDestChannel(), packet.GetSequence())
		if found {
			return sdkerrors.Wrapf(
				types.ErrPacketReceived,
				"destination port: %s, destination channel: %s, sequence: %d", packet.GetDestPort(), packet.GetDestChannel(), packet.GetSequence(),
			)
		}

		k.SetPacketReceipt(ctx, packet.GetDestPort(), packet.GetDestChannel(), packet.GetSequence())
	}

	// log that a packet has been received & executed
	k.Logger(ctx).Info("packet received", "packet", fmt.Sprintf("%v", packet))

	// emit an event that the relayer can query for
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeRecvPacket,
			sdk.NewAttribute(types.AttributeKeyData, string(packet.GetData())),
			sdk.NewAttribute(types.AttributeKeyTimeoutHeight, packet.GetTimeoutHeight().String()),
			sdk.NewAttribute(types.AttributeKeyTimeoutTimestamp, fmt.Sprintf("%d", packet.GetTimeoutTimestamp())),
			sdk.NewAttribute(types.AttributeKeySequence, fmt.Sprintf("%d", packet.GetSequence())),
			sdk.NewAttribute(types.AttributeKeySrcPort, packet.GetSourcePort()),
			sdk.NewAttribute(types.AttributeKeySrcChannel, packet.GetSourceChannel()),
			sdk.NewAttribute(types.AttributeKeyDstPort, packet.GetDestPort()),
			sdk.NewAttribute(types.AttributeKeyDstChannel, packet.GetDestChannel()),
			sdk.NewAttribute(types.AttributeKeyChannelOrdering, channel.Ordering.String()),
		),
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
		),
	})

	return nil
}

// WriteAcknowledgement writes the packet execution acknowledgement to the state,
// which will be verified by the counterparty chain using AcknowledgePacket.
//
// CONTRACT:
//
// 1) For synchronous execution, this function is be called in the IBC handler .
// For async handling, it needs to be called directly by the module which originally
// processed the packet.
//
// 2) Assumes that packet receipt has been writted previously by WriteReceipt.
func (k Keeper) WriteAcknowledgement(
	ctx sdk.Context,
	packet exported.PacketI,
	acknowledgement []byte,
) error {
	// NOTE: IBC app modules might have written the acknowledgement synchronously on
	// the OnRecvPacket callback so we need to check if the acknowledgement is already
	// set on the store and return an error if so.
	if k.HasPacketAcknowledgement(ctx, packet.GetDestPort(), packet.GetDestChannel(), packet.GetSequence()) {
		return types.ErrAcknowledgementExists
	}

	if len(acknowledgement) == 0 {
		return sdkerrors.Wrap(types.ErrInvalidAcknowledgement, "acknowledgement cannot be empty")
	}

	// always set the acknowledgement so that it can be verified on the other side
	k.SetPacketAcknowledgement(
		ctx, packet.GetDestPort(), packet.GetDestChannel(), packet.GetSequence(),
		types.CommitAcknowledgement(acknowledgement),
	)

	// log that a packet has been acknowledged
	k.Logger(ctx).Info("packet acknowledged", "packet", fmt.Sprintf("%v", packet))

	// emit an event that the relayer can query for
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeRecvPacket,
			sdk.NewAttribute(types.AttributeKeyAck, string(acknowledgement)),
		),
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
		),
	})

	return nil
}

// AcknowledgePacket is called by a module to process the acknowledgement of a
// packet previously sent by the calling module on a channel to a counterparty
// module on the counterparty chain. Its intended usage is within the ante
// handler. A subsequent call to AcknowledgementExecuted will clean up the
// packet commitment, which is no longer necessary since the packet has been
// received and acted upon.
func (k Keeper) AcknowledgePacket(
	ctx sdk.Context,
	packet exported.PacketI,
	acknowledgement []byte,
	proof []byte,
	proofHeight exported.Height,
) error {
	channel, found := k.GetChannel(ctx, packet.GetSourcePort(), packet.GetSourceChannel())
	if !found {
		return sdkerrors.Wrapf(
			types.ErrChannelNotFound,
			"port ID (%s) channel ID (%s)", packet.GetSourcePort(), packet.GetSourceChannel(),
		)
	}

	if channel.State != types.OPEN {
		return sdkerrors.Wrapf(
			types.ErrInvalidChannelState,
			"channel state is not OPEN (got %s)", channel.State.String(),
		)
	}

	// NOTE: AcknowledgePacket is called by the AnteHandler which acts upon the packet.Route(),
	// so the capability authentication can be omitted here

	// packet must have been sent to the channel's counterparty
	if packet.GetDestPort() != channel.Counterparty.PortId {
		return sdkerrors.Wrapf(
			types.ErrInvalidPacket,
			"packet destination port doesn't match the counterparty's port (%s ≠ %s)", packet.GetDestPort(), channel.Counterparty.PortId,
		)
	}

	if packet.GetDestChannel() != channel.Counterparty.ChannelId {
		return sdkerrors.Wrapf(
			types.ErrInvalidPacket,
			"packet destination channel doesn't match the counterparty's channel (%s ≠ %s)", packet.GetDestChannel(), channel.Counterparty.ChannelId,
		)
	}

	connectionEnd, found := k.connectionKeeper.GetConnection(ctx, channel.ConnectionHops[0])
	if !found {
		return sdkerrors.Wrap(connectiontypes.ErrConnectionNotFound, channel.ConnectionHops[0])
	}

	if connectionEnd.GetState() != int32(connectiontypes.OPEN) {
		return sdkerrors.Wrapf(
			connectiontypes.ErrInvalidConnectionState,
			"connection state is not OPEN (got %s)", connectiontypes.State(connectionEnd.GetState()).String(),
		)
	}

	commitment := k.GetPacketCommitment(ctx, packet.GetSourcePort(), packet.GetSourceChannel(), packet.GetSequence())

	// verify we sent the packet and haven't cleared it out yet
	if !bytes.Equal(commitment, types.CommitPacket(packet)) {
		return sdkerrors.Wrapf(types.ErrInvalidPacket, "commitment bytes are not equal: got (%v), expected (%v)", types.CommitPacket(packet), commitment)
	}

	if err := k.connectionKeeper.VerifyPacketAcknowledgement(
		ctx, connectionEnd, proofHeight, proof, packet.GetDestPort(), packet.GetDestChannel(),
		packet.GetSequence(), acknowledgement,
	); err != nil {
		return sdkerrors.Wrap(err, "packet acknowledgement verification failed")
	}

	// assert packets acknowledged in order
	if channel.Ordering == types.ORDERED {
		nextSequenceAck, found := k.GetNextSequenceAck(ctx, packet.GetSourcePort(), packet.GetSourceChannel())
		if !found {
			return sdkerrors.Wrapf(
				types.ErrSequenceAckNotFound,
				"source port: %s, source channel: %s", packet.GetSourcePort(), packet.GetSourceChannel(),
			)
		}

		if packet.GetSequence() != nextSequenceAck {
			return sdkerrors.Wrapf(
				sdkerrors.ErrInvalidSequence,
				"packet sequence ≠ next ack sequence (%d ≠ %d)", packet.GetSequence(), nextSequenceAck,
			)
		}
	}

	// NOTE: the remaining code is located in the AcknowledgementExecuted function
	return nil
}

// AcknowledgementExecuted deletes the packet commitment from this chain.
// It is assumed that the acknowledgement verification has already occurred.
//
// CONTRACT: this function must be called in the IBC handler
func (k Keeper) AcknowledgementExecuted(
	ctx sdk.Context,
	chanCap *capabilitytypes.Capability,
	packet exported.PacketI,
) error {
	channel, found := k.GetChannel(ctx, packet.GetSourcePort(), packet.GetSourceChannel())
	if !found {
		return sdkerrors.Wrapf(
			types.ErrChannelNotFound,
			"port ID (%s) channel ID (%s)", packet.GetSourcePort(), packet.GetSourceChannel(),
		)
	}

	capName := host.ChannelCapabilityPath(packet.GetSourcePort(), packet.GetSourceChannel())
	if !k.scopedKeeper.AuthenticateCapability(ctx, chanCap, capName) {
		return sdkerrors.Wrapf(
			types.ErrInvalidChannelCapability,
			"channel capability failed authentication for capability name %s", capName,
		)
	}

	k.deletePacketCommitment(ctx, packet.GetSourcePort(), packet.GetSourceChannel(), packet.GetSequence())

	// increment NextSequenceAck
	if channel.Ordering == types.ORDERED {
		nextSequenceAck, found := k.GetNextSequenceAck(ctx, packet.GetSourcePort(), packet.GetSourceChannel())
		if !found {
			return sdkerrors.Wrapf(
				types.ErrSequenceAckNotFound,
				"source port: %s, source channel: %s", packet.GetSourcePort(), packet.GetSourceChannel(),
			)
		}

		nextSequenceAck++

		// incrementing NextSequenceAck and storing under this chain's channelEnd identifiers
		// Since this is the original sending chain, our channelEnd is packet's source port and channel
		k.SetNextSequenceAck(ctx, packet.GetSourcePort(), packet.GetSourceChannel(), nextSequenceAck)
	}

	// log that a packet has been acknowledged
	k.Logger(ctx).Info(fmt.Sprintf("packet acknowledged: %v", packet))

	// emit an event marking that we have processed the acknowledgement
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeAcknowledgePacket,
			sdk.NewAttribute(types.AttributeKeyTimeoutHeight, packet.GetTimeoutHeight().String()),
			sdk.NewAttribute(types.AttributeKeyTimeoutTimestamp, fmt.Sprintf("%d", packet.GetTimeoutTimestamp())),
			sdk.NewAttribute(types.AttributeKeySequence, fmt.Sprintf("%d", packet.GetSequence())),
			sdk.NewAttribute(types.AttributeKeySrcPort, packet.GetSourcePort()),
			sdk.NewAttribute(types.AttributeKeySrcChannel, packet.GetSourceChannel()),
			sdk.NewAttribute(types.AttributeKeyDstPort, packet.GetDestPort()),
			sdk.NewAttribute(types.AttributeKeyDstChannel, packet.GetDestChannel()),
			sdk.NewAttribute(types.AttributeKeyChannelOrdering, channel.Ordering.String()),
		),
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
		),
	})

	return nil
}