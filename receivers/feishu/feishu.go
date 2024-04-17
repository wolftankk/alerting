package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"time"

	"github.com/grafana/alerting/templates"

	"github.com/bluele/gcache"
	"github.com/grafana/alerting/images"
	"github.com/grafana/alerting/logging"
	"github.com/grafana/alerting/receivers"
	"github.com/prometheus/alertmanager/types"
)

var (
	feishuAPIURL           = "https://open.feishu.cn/open-apis"
	feishuAccessTokenCache = gcache.New(1).Simple().Build()
)

type Notifier struct {
	*receivers.Base
	log        logging.Logger
	tmpl       *templates.Template
	images     images.Provider
	sender     receivers.WebhookSender
	settings   Config
	appVersion string
}

func New(cfg Config, meta receivers.Metadata, template *templates.Template, sender receivers.WebhookSender, images images.Provider, logger logging.Logger, appVersion string) *Notifier {
	return &Notifier{
		Base:     receivers.NewBase(meta),
		settings: cfg,

		images:     images,
		sender:     sender,
		log:        logger,
		tmpl:       template,
		appVersion: appVersion,
	}
}

type feishuImage struct {
	Code    int64  `json:"code"`
	Message string `json:"msg"`
	Data    struct {
		ImageKey string `json:"image_key"`
	} `json:"data"`
}

// https://open.feishu.cn/document/ukTMukTMukTM/uEDO04SM4QjLxgDN
func (fs *Notifier) uploadImage(imagePath string) (string, error) {
	tentantAccessToken, err := fs.getTenantAccessToken()

	if err != nil {
		return "", err
	}

	image, err := os.Open(imagePath)
	if err != nil {
		return "", err
	}
	defer image.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("image", imagePath)
	if err != nil {
		return "", err
	}

	if _, err = io.Copy(part, image); err != nil {
		return "", err
	}

	if err = writer.WriteField("image_type", "message"); err != nil {
		return "", err
	}

	if err = writer.Close(); err != nil {
		return "", err
	}

	request, err := http.NewRequest("POST", feishuAPIURL+"/image/v4/put/", body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", tentantAccessToken))

	client := http.Client{}
	resp, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	imageInfo := &feishuImage{}
	err = json.Unmarshal(b, imageInfo)
	if err != nil {
		return "", err
	}

	return imageInfo.Data.ImageKey, nil
}

type feishuTenant struct {
	Code        int64  `json:"code"`
	Expire      int64  `json:"expire"`
	Message     string `json:"msg"`
	AccessToken string `json:"tenant_access_token"`
}

// https://open.feishu.cn/document/ukTMukTMukTM/uIjNz4iM2MjLyYzM
func (fs *Notifier) getTenantAccessToken() (string, error) {
	k, err := feishuAccessTokenCache.Get("tentant")
	if err == nil {
		return k.(string), nil
	}

	bodyMsg, err := json.Marshal(map[string]string{
		"app_id":     fs.settings.AppID,
		"app_secret": fs.settings.AppSecret,
	})

	if err != nil {
		return "", err
	}

	resp, err := http.Post(feishuAPIURL+"/auth/v3/tenant_access_token/internal/",
		"application/json",
		bytes.NewReader(bodyMsg),
	)

	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)

	if err != nil {
		return "", err
	}

	tenantInfo := &feishuTenant{}

	if err = json.Unmarshal(b, tenantInfo); err != nil {
		return "", err
	}

	if err = feishuAccessTokenCache.SetWithExpire("tentant", tenantInfo.AccessToken, time.Duration(tenantInfo.Expire)*time.Second); err != nil {
		return "", err
	}

	return tenantInfo.AccessToken, nil
}

func (fs *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	fs.log.New("sending feishu")

	var tmplErr error
	tmpl, _ := templates.TmplText(ctx, fs.tmpl, as, fs.log, &tmplErr)

	message := tmpl(fs.settings.Message)
	title := tmpl(fs.settings.Title)

	//build message
	body, err := fs.buildBody(ctx, title, message)
	if err != nil {
		fs.log.Error("gen feishu body faield.", "error", err)
		return false, err
	}

	if tmplErr != nil {
		fs.log.Warn("failed to template Feishu message", "error", tmplErr.Error())
		tmplErr = nil
	}

	cmd := &receivers.SendWebhookSettings{
		URL:        fs.settings.URL,
		Body:       body,
		HTTPMethod: "POST",
	}

	if err = fs.sender.SendWebhook(ctx, cmd); err != nil {
		fs.log.Error("Failed to send feishu", "error", err, "webhook", fs.Name)
		return false, err
	}

	return true, nil
}

func (fs *Notifier) SendResolved() bool {
	return !fs.GetDisableResolveMessage()
}

type feishuTextContent struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	Unescape bool   `json:"un_escape"`
}

type feishuLinkContent struct {
	Tag  string `json:"tag"`
	Text string `json:"text"`
	Link string `json:"href"`
}

type feishuImageContent struct {
	Tag      string `json:"tag"`
	ImageKey string `json:"image_key"`
}

type feishuContent struct {
	MessageType string      `json:"msg_type"`
	Content     interface{} `json:"content"`
}

type feishuPost struct {
	Title   string        `json:"title"`
	Content []interface{} `json:"content"`
}

func (fs *Notifier) buildBody(ctx context.Context, title, msg string) (string, error) {
	var imageContents = make([]interface{}, 0)
	_ = images.WithStoredImages(ctx, fs.log, fs.images, func(idx int, img images.Image) error {
		var imageID, err = fs.uploadImage(img.Path)
		if err != nil {
			fs.log.Error("failed upload image", "error", err, "path", img.Path, "url", img.URL)
			return nil
		}

		imageContents = append(imageContents, feishuImageContent{
			Tag:      "img",
			ImageKey: imageID,
		})

		return nil
	})

	contents := make([]interface{}, 0)

	if len(msg) > 0 {
		subContents := make([]interface{}, 0)
		subContents = append(subContents, feishuTextContent{
			Tag:  "text",
			Text: msg,
		})

		contents = append(contents, subContents)
	}

	if len(imageContents) > 0 {
		contents = append(contents, imageContents)
	}

	ruleURL := receivers.JoinURLPath(fs.tmpl.ExternalURL.String(), "/alerting/list", fs.log)
	if len(ruleURL) > 0 {
		subContents := make([]interface{}, 0)

		subContents = append(subContents, feishuLinkContent{
			Tag:  "a",
			Text: "Alerting list",
			Link: ruleURL,
		})

		contents = append(contents, subContents)
	}

	//if len(fs.settings.MentionUsers) > 0 {
	//}

	post := feishuContent{
		MessageType: "post",
		Content: map[string]interface{}{
			"post": map[string]feishuPost{
				"zh_cn": {
					Title:   title,
					Content: contents,
				},
			},
		},
	}

	p, err := json.Marshal(post)

	if err != nil {
		return "", err
	}

	return string(p), nil
}
