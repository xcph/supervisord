package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jhump/grpctunnel"
	tunnelpb "github.com/jhump/grpctunnel/tunnelpb"
	nodeagentv1 "github.com/xcph/cloudphone-nodeagent-api/pkg/apiv1"
	"github.com/xcph/cloudphone-nodeagent-api/pkg/tunnelmeta"
	"github.com/ochinchina/supervisord/types"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// tunnelServer implements nodeagentv1.SupervisordTunnelServer for grpctunnel reverse tunnel.
type tunnelServer struct {
	nodeagentv1.UnimplementedSupervisordTunnelServer
	sup *Supervisor
}

func (t *tunnelServer) Supervise(_ context.Context, req *nodeagentv1.SuperviseRequest) (*nodeagentv1.SuperviseResponse, error) {
	ok, msg := dispatchSupervise(t.sup, req)
	return &nodeagentv1.SuperviseResponse{Ok: ok, Message: msg}, nil
}

// nodeAgentBridgeLoop connects outbound to cloudphone-node-agent and serves SupervisordTunnel over a reverse tunnel.
// When node-agent restarts or closes the tunnel (e.g. pod rollout), the session ends and this loop
// reconnects with exponential backoff until node-agent accepts a new reverse tunnel.
func nodeAgentBridgeLoop(ctx context.Context, sup *Supervisor) {
	sock := os.Getenv("CLOUDPHONE_NODE_AGENT_SOCKET")
	if sock == "" {
		return
	}
	token := os.Getenv("NODE_AGENT_AUTH_TOKEN")
	podUID := os.Getenv("POD_UID")
	podName := os.Getenv("POD_NAME")
	ns := os.Getenv("POD_NAMESPACE")
	nodeName := os.Getenv("NODE_NAME")
	if podUID == "" || podName == "" || ns == "" {
		log.Warn("node-agent bridge disabled: POD_UID, POD_NAME and POD_NAMESPACE must be set")
		return
	}

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := runReverseTunnelSession(ctx, sup, sock, token, podUID, podName, ns, nodeName); err != nil {
			log.WithError(err).Warn("node-agent reverse tunnel ended")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func runReverseTunnelSession(
	ctx context.Context,
	sup *Supervisor,
	sock, token, podUID, podName, ns, nodeName string,
) error {
	conn, err := grpc.NewClient(
		"unix://"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("grpc dial: %w", err)
	}
	defer conn.Close()

	tStub := tunnelpb.NewTunnelServiceClient(conn)
	rev := grpctunnel.NewReverseTunnelServer(tStub)
	nodeagentv1.RegisterSupervisordTunnelServer(rev, &tunnelServer{sup: sup})

	tctx := metadata.AppendToOutgoingContext(ctx,
		tunnelmeta.MDPodUID, podUID,
		tunnelmeta.MDNamespace, ns,
		tunnelmeta.MDPodName, podName,
		tunnelmeta.MDNodeName, nodeName,
		tunnelmeta.MDAuthToken, token,
	)
	// Blocks until the tunnel is closed (node-agent shutdown, network error, etc.); then returns so the
	// outer loop can dial again after backoff.
	started, err := rev.Serve(tctx)
	if !started {
		return fmt.Errorf("open reverse tunnel: %w", err)
	}
	return err
}

func dispatchSupervise(sup *Supervisor, req *nodeagentv1.SuperviseRequest) (bool, string) {
	switch req.Op {
	case nodeagentv1.ControlOp_CONTROL_OP_START:
		reply := struct{ Success bool }{false}
		err := sup.StartProcess(nil, &StartProcessArgs{Name: req.ProgramName, Wait: true}, &reply)
		if err != nil {
			return false, err.Error()
		}
		return reply.Success, ""
	case nodeagentv1.ControlOp_CONTROL_OP_STOP:
		reply := struct{ Success bool }{false}
		err := sup.StopProcess(nil, &StartProcessArgs{Name: req.ProgramName, Wait: true}, &reply)
		if err != nil {
			return false, err.Error()
		}
		return reply.Success, ""
	case nodeagentv1.ControlOp_CONTROL_OP_RESTART:
		stopReply := struct{ Success bool }{false}
		if err := sup.StopProcess(nil, &StartProcessArgs{Name: req.ProgramName, Wait: true}, &stopReply); err != nil {
			return false, err.Error()
		}
		startReply := struct{ Success bool }{false}
		if err := sup.StartProcess(nil, &StartProcessArgs{Name: req.ProgramName, Wait: true}, &startReply); err != nil {
			return false, err.Error()
		}
		return startReply.Success, ""
	case nodeagentv1.ControlOp_CONTROL_OP_SIGNAL:
		reply := struct{ Success bool }{false}
		err := sup.SignalProcess(nil, &types.ProcessSignal{Name: req.ProgramName, Signal: req.SignalName}, &reply)
		if err != nil {
			return false, err.Error()
		}
		return reply.Success, ""
	case nodeagentv1.ControlOp_CONTROL_OP_LIST_PROGRAMS:
		reply := struct{ AllProcessInfo []types.ProcessInfo }{}
		if err := sup.GetAllProcessInfo(nil, &struct{}{}, &reply); err != nil {
			return false, err.Error()
		}
		b, err := json.Marshal(reply.AllProcessInfo)
		if err != nil {
			return false, err.Error()
		}
		return true, string(b)
	default:
		return false, fmt.Sprintf("unknown op %v", req.Op)
	}
}
