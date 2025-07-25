package sip

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/media-sdk/sdp"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

const (
	testPortSIPMin = 30000
	testPortSIPMax = 30050

	testPortRTPMin = 30100
	testPortRTPMax = 30150
)

func getResponseOrFail(t *testing.T, tx sip.ClientTransaction) *sip.Response {
	select {
	case <-tx.Done():
		t.Fatal("Transaction failed to complete")
	case res := <-tx.Responses():
		return res
	}

	return nil
}

func expectNoResponse(t *testing.T, tx sip.ClientTransaction) {
	select {
	case res := <-tx.Responses():
		t.Fatal("unexpected result:", res)
	case <-time.After(time.Second / 2):
		// ok
	case <-tx.Done():
		// ok
	}
}

type TestHandler struct {
	GetAuthCredentialsFunc func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error)
	DispatchCallFunc       func(ctx context.Context, info *CallInfo) CallDispatch
	OnCallEndFunc          func(ctx context.Context, callInfo *livekit.SIPCallInfo, reason string)
}

func (h TestHandler) GetAuthCredentials(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
	return h.GetAuthCredentialsFunc(ctx, call)
}

func (h TestHandler) DispatchCall(ctx context.Context, info *CallInfo) CallDispatch {
	return h.DispatchCallFunc(ctx, info)
}

func (h TestHandler) GetMediaProcessor(_ []livekit.SIPFeature) msdk.PCM16Processor {
	return nil
}

func (h TestHandler) RegisterTransferSIPParticipantTopic(sipCallId string) error {
	// no-op
	return nil
}

func (h TestHandler) DeregisterTransferSIPParticipantTopic(sipCallId string) {
	// no-op
}

func (h TestHandler) RegisterHoldSIPParticipantTopic(sipCallId string) error {
	// no-op
	return nil
}

func (h TestHandler) DeregisterHoldSIPParticipantTopic(sipCallId string) {
	// no-op
}

func (h TestHandler) RegisterUnholdSIPParticipantTopic(sipCallId string) error {
	// no-op
	return nil
}

func (h TestHandler) DeregisterUnholdSIPParticipantTopic(sipCallId string) {
	// no-op
}

func (h TestHandler) OnCallEnd(ctx context.Context, callInfo *livekit.SIPCallInfo, reason string) {
	if h.OnCallEndFunc != nil {
		h.OnCallEndFunc(ctx, callInfo, reason)
	}
}

func testInvite(t *testing.T, h Handler, hidden bool, from, to string, test func(tx sip.ClientTransaction)) {
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	sipServerAddress := fmt.Sprintf("%s:%d", localIP, sipPort)

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	log := logger.NewTestLogger(t)
	s, err := NewService("", &config.Config{
		HideInboundPort: hidden,
		SIPPort:         sipPort,
		SIPPortListen:   sipPort,
		RTPPort:         rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
	}, mon, log, func(projectID string) rpc.IOInfoClient { return nil })
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(s.Stop)

	s.SetHandler(h)

	require.NoError(t, s.Start())

	sipUserAgent, err := sipgo.NewUA(
		sipgo.WithUserAgent(from),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(s.log))),
	)
	require.NoError(t, err)

	sipClient, err := sipgo.NewClient(sipUserAgent)
	require.NoError(t, err)

	offer, err := sdp.NewOffer(localIP, 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offerData, err := offer.SDP.Marshal()
	require.NoError(t, err)

	inviteRecipent := sip.Uri{User: to, Host: sipServerAddress}
	inviteRequest := sip.NewRequest(sip.INVITE, inviteRecipent)
	inviteRequest.SetDestination(sipServerAddress)
	inviteRequest.SetBody(offerData)
	inviteRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	tx, err := sipClient.TransactionRequest(inviteRequest)
	require.NoError(t, err)
	t.Cleanup(tx.Terminate)

	test(tx)
}

func TestService_AuthFailure(t *testing.T) {
	const (
		expectedFromUser = "foo"
		expectedToUser   = "bar"
	)
	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			require.Equal(t, expectedFromUser, call.From.User)
			require.Equal(t, expectedToUser, call.To.User)
			return AuthInfo{}, fmt.Errorf("Auth Failure")
		},
	}
	testInvite(t, h, false, expectedFromUser, expectedToUser, func(tx sip.ClientTransaction) {
		res := getResponseOrFail(t, tx)
		require.Equal(t, sip.StatusCode(100), res.StatusCode)

		res = getResponseOrFail(t, tx)
		require.Equal(t, sip.StatusCode(503), res.StatusCode)
	})
}

func TestService_AuthDrop(t *testing.T) {
	const (
		expectedFromUser = "foo"
		expectedToUser   = "bar"
	)
	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			require.Equal(t, expectedFromUser, call.From.User)
			require.Equal(t, expectedToUser, call.To.User)
			return AuthInfo{Result: AuthDrop}, nil
		},
	}
	testInvite(t, h, true, expectedFromUser, expectedToUser, func(tx sip.ClientTransaction) {
		expectNoResponse(t, tx)
	})
}

func TestService_OnCallEnd(t *testing.T) {
	const (
		expectedCallID = "test-call-id"
		expectedReason = "test-reason"
	)

	callEnded := make(chan struct{})
	var receivedCallInfo *livekit.SIPCallInfo
	var receivedReason string

	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			return AuthInfo{Result: AuthAccept}, nil
		},
		DispatchCallFunc: func(ctx context.Context, info *CallInfo) CallDispatch {
			return CallDispatch{
				Result: DispatchAccept,
				Room: RoomConfig{
					RoomName: "test-room",
					Participant: ParticipantConfig{
						Identity: "test-participant",
					},
				},
			}
		},
		OnCallEndFunc: func(ctx context.Context, callInfo *livekit.SIPCallInfo, reason string) {
			receivedCallInfo = callInfo
			receivedReason = reason
			close(callEnded)
		},
	}

	// Create a new service
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	log := logger.NewTestLogger(t)
	s, err := NewService("", &config.Config{
		SIPPort:       sipPort,
		SIPPortListen: sipPort,
		RTPPort:       rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
	}, mon, log, func(projectID string) rpc.IOInfoClient { return nil })
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(s.Stop)

	s.SetHandler(h)
	require.NoError(t, s.Start())

	// Call OnCallEnd directly
	h.OnCallEnd(context.Background(), &livekit.SIPCallInfo{
		CallId: expectedCallID,
	}, expectedReason)

	// Wait for OnCallEnd to be called
	select {
	case <-callEnded:
		// Success
	case <-time.After(time.Second):
		t.Fatal("OnCallEnd was not called")
	}

	// Verify the parameters passed to OnCallEnd
	require.NotNil(t, receivedCallInfo, "CallInfo should not be nil")
	require.Equal(t, expectedCallID, receivedCallInfo.CallId, "CallInfo.CallId should match")
	require.Equal(t, expectedReason, receivedReason, "Reason should match")
}
