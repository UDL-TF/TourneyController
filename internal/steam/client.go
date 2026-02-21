package steam

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// baseURL/interface/method/version?parameters
const location = "https://api.steampowered.com/IGameServersService/"
const version = "v1"

// SteamClient is a struct that holds the API key and provides methods to
// interact with the Steam API.
type SteamClient struct {
	apiKey string
}

// NewSteamClient creates a new SteamClient with the provided API key.
func NewSteamClient(apiKey string) *SteamClient {
	return &SteamClient{apiKey: apiKey}
}

// Steam returns a JSON { response: } object, which wraps all return values.
type steamResponse struct {
	Response json.RawMessage `json:"response"`
}

// FML
type serversResponse struct {
	Servers []Account `json:"servers"`
}

// Account is an abstraction around LoginToken, for use with SteamCMD dedicated
// servers.
type Account struct {
	SteamID    string `json:"steamid,omitempty"`
	AppID      uint16 `json:"appid,omitempty"`
	LoginToken string `json:"login_token,omitempty"`
	Memo       string `json:"memo,omitempty"`
	IsDeleted  bool   `json:"is_deleted,omitempty"`
	IsExpired  bool   `json:"is_expired,omitempty"`
	LastLogon  int    `json:"rt_last_logon,omitempty"`
}

// Remove the { response: data } wrapper, and return inner json as byte array.
func unwrapResponse(response *[]byte) error {
	resp := steamResponse{}
	if err := json.Unmarshal(*response, &resp); err != nil {
		return err
	}
	*response = ([]byte)(resp.Response)
	return nil
}

// Wraps requests for Steam Web API, to generalize insertion of API key,
// and handling of Response Header.
func (client *SteamClient) querySteam(command string, method string, params map[string]string) (data []byte, err error) {
	// Prep request
	req, err := http.NewRequest(method, location+command+"/"+version, nil)
	if err != nil {
		return nil, err
	}

	// Add API Key and extra parameters
	q := url.Values{}
	q.Add("key", client.apiKey)
	for key, value := range params {
		q.Add(key, value)
	}

	// Encode parameters and append them to the url
	req.URL.RawQuery = q.Encode()

	// Execute request
	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Drop if Error Header present
	if respErrState := resp.Header.Get("X-error_message"); respErrState != "" {
		return nil, errors.New(respErrState)
	}

	// Check for non-200 status codes
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("steam API request failed with status %d", resp.StatusCode)
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Remove wrapper
	if err = unwrapResponse(&body); err != nil {
		return nil, err
	}

	return body, nil
}

// CreateAccount creates a new game server account and returns the login token for dedicated servers.
func (client *SteamClient) CreateAccount(appID int, memo string) (account Account, err error) {
	// Build query string
	params := make(map[string]string)
	params["appid"] = strconv.Itoa(appID)
	params["memo"] = memo

	// Execute request
	data, err := client.querySteam("CreateAccount", "POST", params)
	if err != nil {
		return account, err
	}

	// Decode response
	if err := json.Unmarshal(data, &account); err != nil {
		return account, err
	}

	return account, nil
}

// GetAccountList returns a list of all accounts.
func (client *SteamClient) GetAccountList() (accounts []Account, err error) {
	data, err := client.querySteam("GetAccountList", "GET", nil)
	if err != nil {
		return accounts, err
	}

	var list serversResponse
	if err := json.Unmarshal(data, &list); err != nil {
		return accounts, err
	}

	accounts = list.Servers
	return accounts, nil
}

// DeleteAccount deletes an account, immediately expiring its LoginToken.
func (client *SteamClient) DeleteAccount(steamID string) error {
	params := make(map[string]string)
	params["steamid"] = steamID

	_, err := client.querySteam("DeleteAccount", "POST", params)
	return err
}

// ResetLoginToken generates a new LoginToken for an existing account.
func (client *SteamClient) ResetLoginToken(steamID string) (account Account, err error) {
	params := make(map[string]string)
	params["steamid"] = steamID

	data, err := client.querySteam("ResetLoginToken", "POST", params)
	if err != nil {
		return account, err
	}

	if err := json.Unmarshal(data, &account); err != nil {
		return account, err
	}

	return account, nil
}
