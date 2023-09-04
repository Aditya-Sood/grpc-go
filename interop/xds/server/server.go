/*
 *
 * Copyright 2021 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Binary server is the server used for xDS interop tests.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/admin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/internal/status"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/xds"

	xdscreds "google.golang.org/grpc/credentials/xds"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

var (
	port             = flag.Int("port", 8080, "Listening port for test service")
	maintenancePort  = flag.Int("maintenance_port", 8081, "Listening port for maintenance services like health, reflection, channelz etc when -secure_mode is true. When -secure_mode is false, all these services will be registered on -port")
	serverID         = flag.String("server_id", "go_server", "Server ID included in response")
	secureMode       = flag.Bool("secure_mode", false, "If true, retrieve security configuration from the management server. Else, use insecure credentials.")
	hostNameOverride = flag.String("host_name_override", "", "If set, use this as the hostname instead of the real hostname")

	logger = grpclog.Component("interop")
)

const (
	rpcBehaviorMdKey  = "rpc-behavior"
	rpcAttemptsMdKey  = "grpc-previous-rpc-attempts"
	sleepPfx          = "sleep-"
	keepOpenVal       = "keep-open"
	errorCodePfx      = "error-code-"
	successOnRetryPfx = "success-on-retry-attempt-"
	hostnamePfx       = "hostname="
)

func getHostname() string {
	if *hostNameOverride != "" {
		return *hostNameOverride
	}
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("failed to get hostname: %v", err)
	}
	return hostname
}

// testServiceImpl provides an implementation of the TestService defined in
// grpc.testing package.
type testServiceImpl struct {
	testgrpc.UnimplementedTestServiceServer
	hostname string
	serverID string
}

func (s *testServiceImpl) EmptyCall(ctx context.Context, _ *testpb.Empty) (*testpb.Empty, error) {
	grpc.SetHeader(ctx, metadata.Pairs("hostname", s.hostname))
	return &testpb.Empty{}, nil
}

func (s *testServiceImpl) UnaryCall(ctx context.Context, in *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	response := &testpb.SimpleResponse{ServerId: s.serverID, Hostname: s.hostname}

	for _, headerVal := range getRPCBehaviorMetadata(ctx) {
		if strings.HasPrefix(headerVal, sleepPfx) {
			sleep, err := strconv.Atoi(headerVal[len(sleepPfx):])
			if err != nil {
				return response, status.Errorf(codes.InvalidArgument, "invalid format for rpc-behavior header: %v", headerVal)
			}

			time.Sleep(time.Duration(sleep) * time.Second)

		} else if strings.HasPrefix(headerVal, keepOpenVal) {
			return nil, nil

		} else if strings.HasPrefix(headerVal, errorCodePfx) {
			errCd, err := strconv.Atoi(headerVal[len(errorCodePfx):])
			if err != nil {
				return response, status.Errorf(codes.InvalidArgument, "invalid format for rpc-behavior header: %v", headerVal)
			}

			return response, status.Errorf(codes.Code(errCd), "rpc failed as per the rpc-behavior header value: %v", headerVal)

		} else if strings.HasPrefix(headerVal, successOnRetryPfx) {
			attempt, err := strconv.Atoi(headerVal[len(successOnRetryPfx):])
			if err != nil {
				return response, status.Errorf(codes.InvalidArgument, "invalid format for rpc-behavior header: %v", headerVal)
			}

			mdRetry := getMetadataValues(ctx, rpcAttemptsMdKey)
			retry, err := strconv.Atoi(mdRetry[0])
			if err != nil {
				return response, status.Errorf(codes.InvalidArgument, "invalid format for grpc-previous-rpc-attempts header: %v", mdRetry[0])
			}

			if retry == attempt {
				break
			}

		} else if strings.HasPrefix(headerVal, hostnamePfx) {
			headerHostname := strings.TrimSpace(headerVal[len(hostnamePfx):])

			if headerHostname != s.hostname {
				break
			}
		}
	}

	grpc.SetHeader(ctx, metadata.Pairs("hostname", s.hostname))
	return response, status.Err(codes.OK, "")
}

func getRPCBehaviorMetadata(ctx context.Context) []string {
	mdRPCBehavior := getMetadataValues(ctx, rpcBehaviorMdKey)
	var rpcBehaviorMetadata []string
	for _, val := range mdRPCBehavior {
		splitVals := strings.Split(val, ",")
		rpcBehaviorMetadata = append(rpcBehaviorMetadata, splitVals...)
	}

	return rpcBehaviorMetadata
}

func getMetadataValues(ctx context.Context, metadataKey string) []string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		logger.Error("Failed to retrieve metadata from incoming RPC context")
		return nil
	}

	return md.Get(metadataKey)
}

// xdsUpdateHealthServiceImpl provides an implementation of the
// XdsUpdateHealthService defined in grpc.testing package.
type xdsUpdateHealthServiceImpl struct {
	testgrpc.UnimplementedXdsUpdateHealthServiceServer
	healthServer *health.Server
}

func (x *xdsUpdateHealthServiceImpl) SetServing(_ context.Context, _ *testpb.Empty) (*testpb.Empty, error) {
	x.healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	return &testpb.Empty{}, nil

}

func (x *xdsUpdateHealthServiceImpl) SetNotServing(_ context.Context, _ *testpb.Empty) (*testpb.Empty, error) {
	x.healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	return &testpb.Empty{}, nil
}

func xdsServingModeCallback(addr net.Addr, args xds.ServingModeChangeArgs) {
	logger.Infof("Serving mode callback for xDS server at %q invoked with mode: %q, err: %v", addr.String(), args.Mode, args.Err)
}

func main() {
	flag.Parse()

	if *secureMode && *port == *maintenancePort {
		logger.Fatal("-port and -maintenance_port must be different when -secure_mode is set")
	}

	testService := &testServiceImpl{hostname: getHostname(), serverID: *serverID}
	healthServer := health.NewServer()
	updateHealthService := &xdsUpdateHealthServiceImpl{healthServer: healthServer}

	// If -secure_mode is not set, expose all services on -port with a regular
	// gRPC server.
	if !*secureMode {
		addr := fmt.Sprintf(":%d", *port)
		lis, err := net.Listen("tcp4", addr)
		if err != nil {
			logger.Fatalf("net.Listen(%s) failed: %v", addr, err)
		}

		server := grpc.NewServer()
		testgrpc.RegisterTestServiceServer(server, testService)
		healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
		healthgrpc.RegisterHealthServer(server, healthServer)
		testgrpc.RegisterXdsUpdateHealthServiceServer(server, updateHealthService)
		reflection.Register(server)
		cleanup, err := admin.Register(server)
		if err != nil {
			logger.Fatalf("Failed to register admin services: %v", err)
		}
		defer cleanup()
		if err := server.Serve(lis); err != nil {
			logger.Errorf("Serve() failed: %v", err)
		}
		return
	}

	// Create a listener on -port to expose the test service.
	addr := fmt.Sprintf(":%d", *port)
	testLis, err := net.Listen("tcp4", addr)
	if err != nil {
		logger.Fatalf("net.Listen(%s) failed: %v", addr, err)
	}

	// Create server-side xDS credentials with a plaintext fallback.
	creds, err := xdscreds.NewServerCredentials(xdscreds.ServerOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		logger.Fatalf("Failed to create xDS credentials: %v", err)
	}

	// Create an xDS enabled gRPC server, register the test service
	// implementation and start serving.
	testServer, err := xds.NewGRPCServer(grpc.Creds(creds), xds.ServingModeCallback(xdsServingModeCallback))
	if err != nil {
		logger.Fatal("Failed to create an xDS enabled gRPC server: %v", err)
	}
	testgrpc.RegisterTestServiceServer(testServer, testService)
	go func() {
		if err := testServer.Serve(testLis); err != nil {
			logger.Errorf("test server Serve() failed: %v", err)
		}
	}()
	defer testServer.Stop()

	// Create a listener on -maintenance_port to expose other services.
	addr = fmt.Sprintf(":%d", *maintenancePort)
	maintenanceLis, err := net.Listen("tcp4", addr)
	if err != nil {
		logger.Fatalf("net.Listen(%s) failed: %v", addr, err)
	}

	// Create a regular gRPC server and register the maintenance services on
	// it and start serving.
	maintenanceServer := grpc.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthgrpc.RegisterHealthServer(maintenanceServer, healthServer)
	testgrpc.RegisterXdsUpdateHealthServiceServer(maintenanceServer, updateHealthService)
	reflection.Register(maintenanceServer)
	cleanup, err := admin.Register(maintenanceServer)
	if err != nil {
		logger.Fatalf("Failed to register admin services: %v", err)
	}
	defer cleanup()
	if err := maintenanceServer.Serve(maintenanceLis); err != nil {
		logger.Errorf("maintenance server Serve() failed: %v", err)
	}
}
