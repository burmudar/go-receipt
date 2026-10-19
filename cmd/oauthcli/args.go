package oauthcli

import (
	"fmt"
)

type OAuthArgs struct {
	ClientID     string
	ClientSecret string
}

func (a *OAuthArgs) Validate() error {
	if len(a.ClientID) == 0 {
		return fmt.Errorf("client-id cannot be empty")
	}
	if len(a.ClientSecret) == 0 {
		return fmt.Errorf("client-secret cannot be empty")
	}
	return nil
}
