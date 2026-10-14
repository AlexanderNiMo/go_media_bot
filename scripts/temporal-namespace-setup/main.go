// setup-namespace creates the Temporal namespace the application expects.
//
// Why a Go binary rather than a shell script calling
// `temporal operator namespace create`: the temporal.io CLI's gRPC client
// (admin-tools:1.32.1 specifically) deadlocks against the matching server
// image pair — TCP works, the call never completes. The Temporal Go SDK's
// generated client goes through immediately; same wire protocol, different
// gRPC stack.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func main() {
	addr := envOr("TEMPORAL_ADDRESS", "temporal:7233")
	ns := envOr("DEFAULT_NAMESPACE", "default")

	conn := waitForTemporal(addr)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := workflowservice.NewWorkflowServiceClient(conn)
	_, err := client.DescribeNamespace(ctx, &workflowservice.DescribeNamespaceRequest{
		Namespace: ns,
	})
	if err == nil {
		log.Printf("namespace %s already exists", ns)
		return
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		log.Fatalf("describe namespace: %v", err)
	}

	_, err = client.RegisterNamespace(ctx, &workflowservice.RegisterNamespaceRequest{
		Namespace:                        ns,
		WorkflowExecutionRetentionPeriod: durationpb.New(7 * 24 * time.Hour),
		// "active" is the single-node cluster name Temporal ships with by
		// default. For multi-cluster setups this would have to come from
		// the cluster config.
		ActiveClusterName: "active",
	})
	if err != nil {
		log.Fatalf("create namespace: %v", err)
	}
	log.Printf("namespace %s created with 7d retention", ns)
}

// waitForTemporal polls until Temporal is reachable AND ready. Dial alone
// is not enough: on a cold start, gRPC connects against a TCP port that
// has been opened by the entrypoint but the gRPC server has not started
// accepting calls yet. GetSystemInfo is the cheapest WorkflowService call,
// and a successful response means the server is actually serving.
func waitForTemporal(addr string) *grpc.ClientConn {
	deadline := time.Now().Add(120 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, err := grpc.DialContext(ctx, addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock())
		cancel()
		if err == nil {
			client := workflowservice.NewWorkflowServiceClient(c)
			ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
			_, err = client.GetSystemInfo(ctx2, &workflowservice.GetSystemInfoRequest{})
			cancel2()
			if err == nil {
				return c
			}
			log.Printf("dial ok but GetSystemInfo failed: %v", err)
			c.Close()
		} else {
			log.Printf("dial %s: %v", addr, err)
		}
		if time.Now().After(deadline) {
			log.Fatalf("could not connect to Temporal at %s within 120s", addr)
		}
		time.Sleep(2 * time.Second)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
