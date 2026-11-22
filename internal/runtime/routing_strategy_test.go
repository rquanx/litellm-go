package runtime

import "testing"

func TestRoutingStrategyKeepsFallbackGroupsOrdered(t *testing.T) {
	zero, one := float64(0), float64(1)
	candidates := []Candidate{
		{DeploymentID: "first", RoutingGroup: "primary", Weight: &zero},
		{DeploymentID: "second", RoutingGroup: "primary", Weight: &one},
		{DeploymentID: "third", RoutingGroup: "fallback"},
	}
	server := NewServer(Config{RoutingStrategy: "simple-shuffle"})
	server.orderDeployments(candidates)
	if candidates[0].DeploymentID != "second" || candidates[2].DeploymentID != "third" {
		t.Fatal("weighted routing changed fallback group precedence")
	}
	server = NewServer(Config{RoutingStrategy: "least-busy"})
	release := server.trackDeployment(candidates[0])
	server.orderDeployments(candidates)
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
	server.orderDeployments(candidates)
	if candidates[0].DeploymentID != "second" || candidates[2].DeploymentID != "third" {
		t.Fatal("cost-based routing did not rank costs within each fallback group")
	}
}
