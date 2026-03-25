//go:build !linux

package main

import (
	nodeagentv1 "github.com/xcph/cloudphone-nodeagent-api/pkg/apiv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (t *tunnelServer) ExecStream(_ grpc.BidiStreamingServer[nodeagentv1.ExecChunk, nodeagentv1.ExecChunk]) error {
	return status.Error(codes.Unimplemented, "ExecStream is only supported on linux")
}
