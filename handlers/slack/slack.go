package slack

import (
	"log"
	"os"
	"strings"

	"github.com/RoadieHQ/kubewise/kwrelease"
	"github.com/RoadieHQ/kubewise/presenters"
	"github.com/slack-go/slack"
	"helm.sh/helm/v3/pkg/release"
)

type Slack struct {
	Token   string
	Channel string
}

func (s *Slack) Init() {
	channel := "#general"
	if value, ok := os.LookupEnv("KW_SLACK_CHANNEL"); ok {
		channel = value
	}

	var token string
	if value, ok := os.LookupEnv("KW_SLACK_TOKEN"); ok {
		token = value
	} else {
		log.Fatalln("Missing environment variable KW_SLACK_TOKEN")
	}

	s.Token = token
	s.Channel = channel
}

func (s *Slack) HandleEvent(releaseEvent *kwrelease.Event) {
	if msg := presenters.PrepareMsg(releaseEvent); msg != "" {
		sendMessage(s, msg)
	}
}

func (s *Slack) HandleServerStartup(releases []*release.Release) {
	if msg := presenters.PrepareServerStartupMsg(releases); msg != "" {
		sendMessage(s, msg)
	}
}

func sendMessage(s *Slack, msg string) {
	api := slack.New(s.Token)
	text := slack.MsgOptionText(msg, false)
	asUser := slack.MsgOptionAsUser(true)

	channelID, timestamp, err := api.PostMessage(s.Channel, text, asUser)

	if err != nil {
		log.Println(strings.ReplaceAll(err.Error(), s.Token, "<slack-api-token>"))
		return
	}

	log.Printf("Message successfully sent to channel %s at %s", channelID, timestamp)
}
