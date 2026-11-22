package runtime

import (
	"context"
	"testing"
	"time"
)

func TestRoutingStrategyKeepsFallbackGroupsOrdered(t *testing.T) {
	zero, one := float64(0), float64(1)
	candidates := []Candidate{
		{DeploymentID: "first", RoutingGroup: "primary", Weight: &zero},
		{DeploymentID: "second", RoutingGroup: "primary", Weight: &one},
		{DeploymentID: "third", RoutingGroup: "fallback"},
	}
	server := NewServer(Config{RoutingStrategy: "simple-shuffle"})
	server.orderDeployments(context.Background(), candidates, false)
	if candidates[0].DeploymentID != "second" || candidates[2].DeploymentID != "third" {
		t.Fatal("weighted routing changed fallback group precedence")
	}
	server = NewServer(Config{RoutingStrategy: "least-busy"})
	release := server.trackDeployment(candidates[0])
	server.orderDeployments(context.Background(), candidates, false)
	if candidates[0].DeploymentID != "first" || candidates[2].DeploymentID != "third" {
		t.Fatal("least-busy did not choose the idle deployment in the primary group")
	}
	release()
	if len(server.deploymentTraffic) != 0 {
		t.Fatal("completed deployment was retained in the traffic table")
	}

	server = NewServer(Config{RoutingStrategy: "cost-based-routing"})
	candidates[0].TokenPrice = 1
	candidates[1].TokenPrice = 0.5
	candidates[2].TokenPrice = 0
	server.orderDeployments(context.Background(), candidates, false)
	if candidates[0].DeploymentID != "second" || candidates[2].DeploymentID != "third" {
		t.Fatal("cost-based routing did not rank costs within each fallback group")
	}
}

func TestLatencyRoutingUsesSeparateRollingStreamSamples(t *testing.T) {
	server := NewServer(Config{RoutingStrategy: "latency-based-routing", LatencySamples: 2})
	fast := Candidate{DeploymentID: "fast", RoutingGroup: "primary"}
	slow := Candidate{DeploymentID: "slow", RoutingGroup: "primary"}
	server.recordLatency(fast, false, 100*time.Millisecond)
	server.recordLatency(fast, false, 10*time.Millisecond)
	server.recordLatency(fast, false, 10*time.Millisecond)
	server.recordLatency(slow, false, 20*time.Millisecond)
	server.recordLatency(fast, true, 30*time.Millisecond)
	server.recordLatency(slow, true, 5*time.Millisecond)
	group := []Candidate{slow, fast}
	server.orderDeployments(context.Background(), group, false)
	if group[0].DeploymentID != fast.DeploymentID {
		t.Fatal("non-streaming selection did not use the rolling response latency")
	}
	server.orderDeployments(context.Background(), group, true)
	if group[0].DeploymentID != slow.DeploymentID {
		t.Fatal("streaming selection did not use time to first data")
	}
}
