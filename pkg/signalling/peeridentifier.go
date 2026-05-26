package signalling

import "github.com/google/uuid"

type PeerIdentifier struct {
	Uuid     uuid.UUID
	PublicIP string
	Name     string
}

type PeerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
