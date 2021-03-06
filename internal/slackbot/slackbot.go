package slackbot

import (
	"coffeeBot/internal/api"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ajg/form"
	"github.com/google/uuid"
	"github.com/nlopes/slack"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	ActionJoin = "join"
)

func New(cfg *Config) (api.Interface, error) {
	slackApi := slack.New(cfg.AccessToken)
	if r, err := slackApi.AuthTest(); err != nil {
		return nil, err
	} else {
		log.Println("[Slack] Authenticated for team", r.Team, "as user", r.User)
	}
	if err := slackApi.SetUserAsActive(); err != nil {
		return nil, err
	}
	return &bot{
		client: slackApi,
		rounds: make(map[string]*api.CoffeeRound),
		mux:    sync.Mutex{},
	}, nil
}

func (b *bot) HandleCommand(w http.ResponseWriter, r *http.Request) {
	p, err := decodePayload(r)
	if err != nil {
		log.Println("[Slack] Failed to decode payload.", err)
		return
	}

	if p.InteractionJson != "" {
		var i interaction
		if err := json.Unmarshal([]byte(p.InteractionJson), &i); err != nil {
			log.Println("[Slack] Failed to parse interaction json.", err)
			w.WriteHeader(http.StatusBadRequest)
		}
		b.handleInteraction(w, &i)
		return
	}

	log.Println("[Slack] Handling", p.Command, p.Text)
	switch p.Command {
	case "/coffee":
		b.handleCoffeeCommand(&p)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func decodePayload(r *http.Request) (p payload, err error) {
	d := form.NewDecoder(r.Body)
	d.IgnoreUnknownKeys(true)
	err = d.Decode(&p)
	return
}

func (b *bot) addRound(id string, r *api.CoffeeRound) {
	b.mux.Lock()
	defer b.mux.Unlock()
	b.rounds[id] = r
}

func (b *bot) removeRound(id string) {
	b.mux.Lock()
	defer b.mux.Unlock()
	delete(b.rounds, id)
}

func (b *bot) joinRound(id string, info api.User) (*api.CoffeeRound, error) {
	b.mux.Lock()
	defer b.mux.Unlock()
	r, ok := b.rounds[id]
	if !ok {
		return nil, errors.New(fmt.Sprint("Round not found with id: ", id))
	}
	return r, r.Join(info)
}

func (b *bot) handleCoffeeCommand(p *payload) {
	args := strings.Split(p.Text, " ")
	if len(args) < 2 || args[0] == "" || args[1] == "" {
		err := errors.New("dialog not implemented yet. Use `/coffee {milk} {slots} [minutes]` instead")
		b.postEphemeralError(p.ChannelId, p.UserId, p.ChannelName, p.UserName, err)
		return
	}

	milk, err := parseMilkType(args[0])
	if err != nil {
		b.postEphemeralError(p.ChannelId, p.UserId, p.ChannelName, p.UserName, err)
		return
	}

	slots, err := strconv.Atoi(args[1])
	if err != nil {
		b.postEphemeralError(p.ChannelId, p.UserId, p.ChannelName, p.UserName, err)
		return
	}
	if slots <= 0 {
		msg := ":no_entry: Just making a coffee for yourself, eh? :shame: No need to rub it in. :stuck_out_tongue:"
		b.postEphemeral(p.ChannelId, p.UserId, p.ChannelName, p.UserName, msg)
		return
	}

	minutes := -1
	if len(args) > 2 && args[2] != "" {
		minutes, err = strconv.Atoi(args[1])
		if err != nil {
			b.postEphemeralError(p.ChannelId, p.UserId, p.ChannelName, p.UserName, err)
			return
		}
	}

	id := uuid.New().String()
	cr := api.NewCoffeeRound(api.User{
		ID:       p.UserId,
		Username: p.UserName,
		Name:     p.UserName,
	}, milk, slots, minutes)
	b.addRound(id, cr)

	if _, _, err := b.client.PostMessage(p.ChannelId, slack.MsgOptionBlocks(b.buildBlocks(id, cr)...)); err != nil {
		b.postEphemeralError(p.ChannelId, p.UserId, p.ChannelName, p.UserName, err)
	}
}

func (b *bot) handleInteraction(w http.ResponseWriter, i *interaction) {
	log.Println("[Slack] Received interaction from", i.User.Name)

	if len(i.Actions) < 1 {
		w.WriteHeader(http.StatusBadRequest)
	}

	switch a := i.Actions[0]; a.ActionID {
	case ActionJoin:
		log.Println("[Slack]", i.User.Name, "would like to join the coffee round:", a.Value)
		if round, err := b.joinRound(a.Value, i.User); err != nil {
			b.postEphemeralError(i.Channel.ID, i.User.ID, i.Channel.Name, i.User.Name, err)
		} else {
			_, _, _, err = b.client.UpdateMessage(i.Channel.ID, i.Container.MessageTs, slack.MsgOptionBlocks(b.buildBlocks(a.Value, round)...))
			if err != nil {
				b.postEphemeralError(i.Channel.ID, i.User.ID, i.Channel.Name, i.User.Name, err)
			}

			if round.AvailableSlots() <= 0 {
				msg := fmt.Sprintf("@%s :information_desk_person: ", round.Creator.Username)
				n := len(round.Joiners)
				if n > 1 {
					names := make([]string, len(round.Joiners))
					for i, v := range round.Joiners {
						names[i] = v.Name
					}
					msg += fmt.Sprintf("%s and %s have joined your round.", strings.Join(names[:n-1], ", "), names[n-1])
				} else if n < 1 {
					msg += "Nobody joined your round."
				} else {
					msg += fmt.Sprintf("%s is the only person to join your round.", round.Joiners[0].Name)
				}
				b.postEphemeral(i.Channel.ID, round.Creator.ID, i.Channel.Name, round.Creator.Name, msg)
			}
		}
	default:
		log.Println("[Slack] Unrecognised action id:", a.ActionID)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (b *bot) postEphemeralError(channelId, userId, channelName, userName string, err error) {
	log.Println("[Slack]", err)
	b.postEphemeral(channelId, userId, channelName, userName, err.Error())
}

func (b *bot) postEphemeral(channelId, userId, channelName, userName string, msg string) {
	if _, err := b.client.PostEphemeral(channelId, userId, slack.MsgOptionText(msg, false), slack.MsgOptionParse(true)); err != nil {
		log.Println("[Slack] Failed to send message to", userName, "on", channelName, err)
	}
}

func (b *bot) buildBlocks(id string, cr *api.CoffeeRound) (blocks []slack.Block) {
	headingText := fmt.Sprintf(":coffee: *@%s is making coffee", cr.Creator.Name)
	if cr.Minutes < 0 {
		headingText += ".*"
	} else {
		headingText += fmt.Sprintf(" in %d minutes.*", 5)
	}
	headingBlock := slack.NewTextBlockObject(slack.MarkdownType, headingText, false, false)

	var slotsText string
	as := cr.AvailableSlots()
	if as > 1 {
		slotsText = fmt.Sprintf("There is space for %d more people.", cr.AvailableSlots())
	} else if as == 1 {
		slotsText = "There is space of 1 more person."
	} else {
		slotsText = "All slots in this round have been filled!"
	}
	slotsBlock := slack.NewTextBlockObject(slack.PlainTextType, slotsText, false, false)

	joinButton := slack.NewButtonBlockElement(ActionJoin, id, slack.NewTextBlockObject(slack.PlainTextType, string(cr.Milk), true, false))

	blocks = []slack.Block{
		slack.NewSectionBlock(headingBlock, nil, nil),
		slack.NewDividerBlock(),
		slack.NewSectionBlock(slotsBlock, nil, slack.NewAccessory(joinButton)),
	}

	if len(cr.Joiners) > 0 {

		var joinerElems []slack.MixedElement

		for i := range cr.Joiners {
			if info, err := b.client.GetUserInfo(cr.Joiners[i].ID); err != nil {
				log.Println("[Slack]", err)
				joinerElems = append(joinerElems, slack.NewTextBlockObject(slack.PlainTextType, cr.Joiners[i].Name, false, false))
			} else {
				joinerElems = append(joinerElems, slack.NewImageBlockElement(info.Profile.Image48, info.Name))
			}
		}

		blocks = append(blocks, slack.NewContextBlock("", joinerElems...))
	}
	return
}

func parseMilkType(milk string) (api.MilkType, error) {
	switch milk {
	case ":cow:":
		fallthrough
	case "dairy":
		return api.MilkTypeDairy, nil
	case ":ear_of_rice:":
		fallthrough
	case ":rice_crop:":
		fallthrough
	case "oat":
		return api.MilkTypeOat, nil
	default:
		return "", errors.New(fmt.Sprintf("%s not recognised a milk type", milk))
	}
}
