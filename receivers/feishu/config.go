package feishu

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/grafana/alerting/templates"
)

type Config struct {
	URL         string `json:"url,omitempty" yaml:"url,omitempty"`
	AppID       string `json:"appId,omitempty" yaml:"appId,omitempty"`
	AppSecret   string `json:"appSecret,omitempty" yaml:"appSecret,omitempty"`
	MessageType string `json:"msgType,omitempty" yaml:"msgType,omitempty"`
	Title       string `json:"title,omitempty" yaml:"title,omitempty"`
	Message     string `json:"message,omitempty" yaml:"message,omitempty"`
}

const defaultFeishuMsgType = "post"

func NewConfig(jsonData json.RawMessage) (Config, error) {
	var settings Config
	err := json.Unmarshal(jsonData, &settings)

	if err != nil {
		return Config{}, fmt.Errorf("failed to unmarshal settings: %w", err)
	}

	url := settings.URL
	appId := settings.AppID
	appSecret := settings.AppSecret

	if url == "" || appId == "" || appSecret == "" {
		return Config{}, errors.New("could not find Bot AppID or AppSecret in settings")
	}

	if settings.Title == "" {
		settings.Title = templates.DefaultMessageTitleEmbed
	}

	if settings.Message == "" {
		settings.Message = templates.DefaultMessageEmbed
	}

	return settings, nil
}
