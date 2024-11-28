package feishu

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/grafana/alerting/images"
	"github.com/grafana/alerting/logging"
	"github.com/grafana/alerting/receivers"
	"github.com/grafana/alerting/templates"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/prometheus/alertmanager/types"
)

const feishuAPIURL = "https://open.feishu.cn/open-apis"

var (
	feishuClient = &http.Client{
		Timeout: time.Second * 30,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Renegotiation: tls.RenegotiateFreelyAsClient,
			},
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
		},
	}
)

type Notifier struct {
	*receivers.Base
	log        logging.Logger
	tmpl       *templates.Template
	images     images.Provider
	sender     receivers.WebhookSender
	settings   Config
	larkClient *lark.Client
	appVersion string
}

func New(cfg Config, meta receivers.Metadata, template *templates.Template, sender receivers.WebhookSender, images images.Provider, logger logging.Logger, appVersion string) *Notifier {
	client := lark.NewClient(cfg.AppID, cfg.AppSecret, lark.WithHttpClient(feishuClient))

	return &Notifier{
		Base:     receivers.NewBase(meta),
		settings: cfg,

		images:     images,
		sender:     sender,
		log:        logger,
		tmpl:       template,
		appVersion: appVersion,
		larkClient: client,
	}
}

// https://open.feishu.cn/document/ukTMukTMukTM/uEDO04SM4QjLxgDN
func (fs *Notifier) uploadImage(imagePath string) (string, error) {
	image, err := os.Open(imagePath)
	if err != nil {
		return "", err
	}
	defer image.Close()

	req := larkim.NewCreateImageReqBuilder().Body(
		larkim.NewCreateImageReqBodyBuilder().
			ImageType("message").
			Image(image).
			Build(),
	).Build()

	resp, err := fs.larkClient.Im.Image.Create(context.Background(), req)

	if err != nil {
		return "", err
	}

	if !resp.Success() {
		return "", errors.New(fmt.Sprintf("logId: %s, error response: \n%s", resp.RequestId(), larkcore.Prettify(resp.CodeError)))
	}

	return *resp.Data.ImageKey, nil
}

// from email / phone -> openid
func (fs *Notifier) getUserIDs(emails []string) ([]string, error) {
	//check email is valid, if == all return
	for _, email := range emails {
		if email == "all" {
			return []string{"all"}, nil
		}
	}

	req := larkcontact.NewBatchGetIdUserReqBuilder().
		UserIdType("open_id").
		Body(
			larkcontact.NewBatchGetIdUserReqBodyBuilder().
				Emails(emails).
				Build(),
		).
		Build()

	resp, err := fs.larkClient.Contact.User.BatchGetId(context.Background(), req)

	if err != nil {
		return nil, err
	}

	if !resp.Success() {
		return nil, errors.New(fmt.Sprintf("logId: %s, error response: \n%s", resp.RequestId(), larkcore.Prettify(resp.CodeError)))
	}

	//return resp.Data.UserList
	userIds := make([]string, len(resp.Data.UserList))

	for idx, c := range resp.Data.UserList {
		userIds[idx] = *c.UserId
	}

	return userIds, nil
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

type feishuAt struct {
	Tag    string `json:"tag"`
	UserId string `json:"user_id"`
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

	if len(fs.settings.MentionUsers) > 0 {

		mentionUsers, err := fs.getUserIDs(fs.settings.MentionUsers)
		if err != nil {
			//not at
		} else {
			subContents := make([]interface{}, len(mentionUsers))

			for idx, userId := range mentionUsers {
				subContents[idx] = feishuAt{
					Tag:    "at",
					UserId: userId,
				}
			}

			contents = append(contents, subContents)
		}
	}

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
