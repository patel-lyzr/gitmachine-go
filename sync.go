package gitmachine

import (
	"context"
	"fmt"
	"log"
	"time"
)

// SyncResult reports what changed during an AWS sync.
type SyncResult struct {
	Added   []string // Instance IDs added to local state
	Updated []string // Instance IDs whose status/IP was updated
	Removed []string // Instance IDs removed from local state (terminated)
}

// SyncNodesFromAWS discovers all gitmachine-tagged EC2 instances and reconciles
// them with the local nodes.json state. It adds missing instances, updates
// stale IPs/statuses, and optionally removes records for terminated instances.
func SyncNodesFromAWS(ctx context.Context, provider *EC2Provider, state *NodeState) (*SyncResult, error) {
	discovered, err := provider.DiscoverInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover instances: %w", err)
	}

	result := &SyncResult{}

	// Build a set of discovered instance IDs for quick lookup.
	discoveredIDs := make(map[string]*DiscoveredInstance, len(discovered))
	for i := range discovered {
		discoveredIDs[discovered[i].ID] = &discovered[i]
	}

	// Update existing nodes that are also in AWS.
	for i := range state.Nodes {
		node := &state.Nodes[i]
		if node.Provider != "aws" {
			continue
		}
		if di, ok := discoveredIDs[node.ID]; ok {
			changed := false
			// Update IP if changed.
			if di.PublicIP != node.PublicIP {
				node.PublicIP = di.PublicIP
				changed = true
			}
			// Update status.
			newStatus := awsStateToStatus(di.State)
			if newStatus != node.Status {
				node.Status = newStatus
				changed = true
			}
			if changed {
				_ = state.Update(node.ID, func(n *NodeRecord) {
					n.PublicIP = di.PublicIP
					n.Status = newStatus
				})
				result.Updated = append(result.Updated, node.ID)
			}
			// Mark as handled.
			delete(discoveredIDs, node.ID)
		}
	}

	// Add newly discovered instances not in local state.
	for _, di := range discoveredIDs {
		name := di.Tags["Name"]
		if name == "" {
			name = "gitmachine"
		}

		record := NodeRecord{
			ID:           di.ID,
			Provider:     "aws",
			InstanceType: di.InstanceType,
			Region:       di.Region,
			PublicIP:     di.PublicIP,
			SSHUser:      defaultSSHUser,
			SSHKeyName:   di.KeyName,
			Status:       awsStateToStatus(di.State),
			CreatedAt:    di.LaunchTime,
			Tags:         di.Tags,
		}

		// Check if we have a local SSH key for this instance.
		keyPath := state.KeyPath(di.ID)
		if keyPath != "" {
			record.SSHKeyPath = keyPath
		}

		if err := state.Add(record); err != nil {
			log.Printf("sync: failed to add node %s: %v", di.ID, err)
			continue
		}
		result.Added = append(result.Added, di.ID)
	}

	return result, nil
}

func awsStateToStatus(awsState string) string {
	switch awsState {
	case "running", "pending":
		return "running"
	case "stopped", "stopping":
		return "stopped"
	case "terminated", "shutting-down":
		return "terminated"
	default:
		return "unknown"
	}
}

// SyncNodesFromAWSWithCreds is a convenience wrapper that creates an EC2Provider
// from stored credentials and runs the sync.
func SyncNodesFromAWSWithCreds(ctx context.Context) (*SyncResult, *NodeState, error) {
	state, err := NewNodeState()
	if err != nil {
		return nil, nil, fmt.Errorf("load node state: %w", err)
	}

	config := &EC2MachineConfig{}
	store, err := NewCredentialStore()
	if err == nil {
		if cred := store.GetDefault("aws"); cred != nil {
			config.AccessKeyID = cred.Fields["access_key_id"]
			config.SecretAccessKey = cred.Fields["secret_access_key"]
			config.Region = cred.Fields["region"]
		}
	}

	provider := NewEC2Provider(config)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	result, err := SyncNodesFromAWS(ctx, provider, state)
	if err != nil {
		return nil, state, err
	}

	return result, state, nil
}
