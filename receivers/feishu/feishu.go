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

	"github.com/grafana/alerting/images"
	"github.com/grafana/alerting/logging"
	"github.com/grafana/alerting/receivers"
	"github.com/grafana/alerting/templates"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/common/model"
)

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

func (fs *Notifier) Notify(ctx context.Context, alerts ...*types.Alert) (bool, error) {
	fs.log.Info("sending feishu")

	//build message
	body, err := fs.buildBody(ctx, alerts...)

	fs.log.Info("feishu body ", body)
	if err != nil {
		fs.log.Error("gen feishu body faield.", "error", err)
		return false, err
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

type feishuCard struct {
	Header   *feishuHeader   `json:"header"`
	CardLink *feishuCardLink `json:"card_link,omitempty"`
	Elements any             `json:"elements"`
}

type feishuCardLink struct {
	Url string `json:"url"`
}

type feishuHeader struct {
	Title    *feishuPlainText  `json:"title"`
	Template string            `json:"template,omitempty"`
	Icon     *feishuHeaderIcon `json:"ud_icon,omitempty"`
}

type feishuHeaderIcon struct {
	Token string `json:"token"`
}

type feishuPlainText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type feishuImageList struct {
	Tag             string               `json:"tag"`
	CombinationMode string               `json:"combination_mode"`
	ImageList       []feishuImageElement `json:"img_list"`
}

type feishuImageElement struct {
	ImageKey string `json:"img_key"`
}

type feishuPost struct {
	MessageType string      `json:"msg_type"`
	Card        *feishuCard `json:"card"`
}

type feishuMention struct {
	Tag    string `json:"tag"`
	UserId string `json:"user_id"`
}

func (fs *Notifier) buildBody(ctx context.Context, alerts ...*types.Alert) (string, error) {
	var tmplErr error
	tmpl, _ := templates.TmplText(ctx, fs.tmpl, alerts, fs.log, &tmplErr)

	message := tmpl(fs.settings.Message)
	title := tmpl(fs.settings.Title)

	if tmplErr != nil {
		fs.log.Warn("failed to template Feishu message", "error", tmplErr.Error())
		tmplErr = nil
	}

	alertStatus := types.Alerts(alerts...).Status()

	card := &feishuCard{}

	header := &feishuHeader{
		Title: &feishuPlainText{
			Tag:     "plain_text",
			Content: title,
		},
		Template: "default",
	}

	if alertStatus == model.AlertFiring {
		header.Template = "red"
		header.Icon = &feishuHeaderIcon{
			Token: "warning_outlined",
		}
	} else if alertStatus == model.AlertResolved {
		header.Template = "green"
		header.Icon = &feishuHeaderIcon{
			Token: "resolve_outlined",
		}
	}

	card.Header = header

	//ruleURL := receivers.JoinURLPath(fs.tmpl.ExternalURL.String(), "/alerting/list", fs.log)
	//if len(ruleURL) > 0 {
	//	card.CardLink = &feishuCardLink{
	//		Url: ruleURL,
	//	}
	//}

	contents := make([]interface{}, 0)
	if len(message) > 0 {
		contents = append(contents, feishuPlainText{
			Tag:     "markdown",
			Content: message,
		})
	}

	var imageContents = make([]feishuImageElement, 0)
	_ = images.WithStoredImages(ctx, fs.log, fs.images, func(idx int, img images.Image) error {
		var imageID, err = fs.uploadImage(img.Path)
		if err != nil {
			fs.log.Error("failed upload image", "error", err, "path", img.Path, "url", img.URL)
			return nil
		}

		imageContents = append(imageContents, feishuImageElement{
			ImageKey: imageID,
		})

		return nil
	}, alerts...)

	if len(imageContents) > 0 {
		contents = append(contents, feishuImageList{
			Tag:             "img_combination",
			CombinationMode: "bisect",
			ImageList:       imageContents,
		})
	}

	appendSpace := func() {
		if len(contents) > 0 {
			contents = append(contents, struct {
				Tag string `json:"tag"`
			}{
				Tag: "hr",
			})
		}
	}

	if len(fs.settings.MentionUsers) > 0 {
		appendSpace()

		mentionUsers, err := fs.getUserIDs(fs.settings.MentionUsers)
		if err != nil {
			//not at
		} else {
			subContents := make([]interface{}, len(mentionUsers))

			for idx, userId := range mentionUsers {
				subContents[idx] = feishuMention{
					Tag:    "at",
					UserId: userId,
				}
			}

			contents = append(contents, subContents)
		}

	}

	card.Elements = contents

	post := &feishuPost{
		MessageType: fs.settings.MessageType,
		Card:        card,
	}

	p, err := json.Marshal(post)

	if err != nil {
		return "", err
	}

	return string(p), nil
}
