package oauthcli

import (
	"fmt"

	"github.com/spf13/cobra"
)

type OAuthArgs struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

func NewOAuthArgs(id, secret, redirectURI string) *OAuthArgs {
	return &OAuthArgs{
		ClientID:     id,
		ClientSecret: secret,
		RedirectURI:  redirectURI,
	}
}

func (a *OAuthArgs) With(cmd *cobra.Command) (*OAuthArgs, error) {
	if v, err := cmd.Flags().GetString("client-id"); err != nil {
		return nil, fmt.Errorf("client-id not set: %w", err)
	} else {
		a.ClientID = v
	}
	if v, err := cmd.Flags().GetString("client-secret"); err != nil {
		return nil, fmt.Errorf("client-secret not set: %w", err)
	} else {
		a.ClientSecret = v
	}
	if v, err := cmd.Flags().GetString("redirect-uri"); err != nil {
		return nil, fmt.Errorf("redirect-uri not set: %w", err)
	} else {
		a.RedirectURI = v
	}
	return a, nil
}

func (a *OAuthArgs) Validate() error {
	if len(a.ClientID) == 0 {
		return fmt.Errorf("client-id cannot be empty")
	}
	if len(a.ClientSecret) == 0 {
		return fmt.Errorf("client-secret cannot be empty")
	}
	if len(a.RedirectURI) == 0 {
		return fmt.Errorf("redirect-uri cannot be empty")
	}
	return nil
}
