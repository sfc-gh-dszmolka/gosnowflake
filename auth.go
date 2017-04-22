// Go Snowflake Driver - Snowflake driver for Go's database/sql package
//
// Copyright (c) 2017 Snowflake Computing Inc. All right reserved.
//

package gosnowflake

import (
	"encoding/json"
	"log"
	"net/url"
	"time"
)

type AuthRequestClientEnvironment struct {
	Application string `json:"APPLICATION"`
	OsVersion   string `json:"OS_VERSION"`
}
type AuthRequestData struct {
	ClientAppId       string                       `json:"CLIENT_APP_ID"`
	ClientAppVersion  string                       `json:"CLIENT_APP_VERSION"`
	SvnRevision       string                       `json:"SVN_REVISION"`
	AccoutName        string                       `json:"ACCOUNT_NAME"`
	LoginName         string                       `json:"LOGIN_NAME,omitempty"`
	Password          string                       `json:"PASSWORD,omitempty"`
	RawSAMLResponse   string                       `json:"RAW_SAML_RESPONSE,omitempty"`
	ExtAuthnDuoMethod string                       `json:"EXT_AUTHN_DUO_METHOD,omitempty"`
	Passcode          string                       `json:"PASSCODE,omitempty"`
	ClientEnvironment AuthRequestClientEnvironment `json:"CLIENT_ENVIRONMENT"`
}
type AuthRequest struct {
	Data AuthRequestData `json:"data"`
}

type AuthResponseParameter struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}

type AuthResponseSessionInfo struct {
	DatabaseName  string `json:"databaseName"`
	SchemaName    string `json:"schemaName"`
	WarehouseName string `json:"warehouseName"`
	RoleName      string `json:"roleName"`
}

type AuthResponseMain struct {
	Token                   string                  `json:"token,omitempty"`
	ValidityInSeconds       time.Duration           `json:"validityInSeconds,omitempty"`
	MasterToken             string                  `json:"maxterToken,omitempty"`
	MasterValidityInSeconds time.Duration           `json:"masterValidityInSeconds"`
	DisplayUserName         string                  `json:"displayUserName"`
	ServerVersion           string                  `json:"serverVersion"`
	FirstLogin              bool                    `json:"firstLogin"`
	RemMeToken              string                  `json:"remMeToken"`
	RemMeValidityInSeconds  time.Duration           `json:"remMeValidityInSeconds"`
	HealthCheckInterval     time.Duration           `json:"healthCheckInterval"`
	NewClientForUpgrade     string                  `json:"newClientForUpgrade"` // TODO: what is datatype?
	SessionId               int                     `json:"sessionId"`
	Parameters              []AuthResponseParameter `json:"parameters"`
	SessionInfo             AuthResponseSessionInfo `json:"sessionInfo"`
}
type AuthResponse struct {
	Data    AuthResponseMain `json:"data"`
	Message string           `json:"message"`
	Code    string           `json:"code"`
	Success bool             `json:"success"`
}

func Authenticate(
  sr *snowflakeRestful,
  user string,
  password string,
  account string,
  database string,
  schema string,
  warehouse string,
  role string,
  passcode string,
  passcodeInPassword bool,
  samlResponse string,
  mfaCallback string,
  passwordCallback string,
  sessionParameters map[string]string) (resp *AuthResponseSessionInfo, err error) {
	log.Println("Authenticate")

	if sr.Token != "" && sr.MasterToken != "" {
		log.Println("Tokens are already available.")
		return nil, nil
	}

	headers := make(map[string]string)
	headers["Content-Type"] = ContentTypeApplicationJson
	headers["accept"] = AcceptTypeAppliationSnowflake
	headers["User-Agent"] = UserAgent

	clientEnvironment := AuthRequestClientEnvironment{
		Application: ClientType,
		OsVersion:   OSVersion,
	}

	requestMain := AuthRequestData{
		ClientAppId:       ClientType,
		ClientAppVersion:  ClientVersion,
		SvnRevision:       "",
		AccoutName:        account,
		ClientEnvironment: clientEnvironment,
	}
	if samlResponse != "" {
		requestMain.RawSAMLResponse = samlResponse
	} else {
		requestMain.LoginName = user
		requestMain.Password = password
		switch {
		case passcodeInPassword:
			requestMain.ExtAuthnDuoMethod = "passcode"
		case passcode != "":
			requestMain.Passcode = passcode
			requestMain.ExtAuthnDuoMethod = "passcode"
		}
	}

	authRequest := AuthRequest{
		Data: requestMain,
	}
	params := &url.Values{}
	if database != "" {
		params.Add("databaseName", url.QueryEscape(database))
	}
	if schema != "" {
		params.Add("schemaName", url.QueryEscape(schema))
	}
	if warehouse != "" {
		params.Add("warehouse", url.QueryEscape(warehouse))
	}
	if role != "" {
		params.Add("roleName", url.QueryEscape(role))
	}

	var json_body []byte
	json_body, err = json.Marshal(authRequest)
	if err != nil {
		return
	}

	log.Printf("PARAMS for Auth: %v", params)
	respd, err := sr.PostAuth(params, headers, json_body, sr.LoginTimeout)
	if err != nil {
		// TODO: error handing, Forbidden 403, BadGateway 504, ServiceUnavailable 503
		return nil, err
	}
	if respd.Success {
		log.Println("Authentication SUCCES")
		sr.Token = respd.Data.Token
		sr.MasterToken = respd.Data.MasterToken
		sr.SessionId = respd.Data.SessionId
	} else {
		log.Println("Authentication FAILED")
		sr.Token = ""
		sr.MasterToken = ""
		sr.SessionId = -1
	}

	return &respd.Data.SessionInfo, nil
}