package slack

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/slack-go/slack"
	"github.com/target/goalert/config"
	"github.com/target/goalert/notification"
	"github.com/target/goalert/permission"
	"github.com/target/goalert/user"
	"github.com/target/goalert/util/log"
)

// UserGroupSender processes on-call notifications by updating the members of a Slack user group.
type UserGroupSender struct {
	*ChannelSender
}

var _ notification.Sender = (*UserGroupSender)(nil)

// UserGroupSender returns a new UserGroupSender wrapping the given ChannelSender.
func (s *ChannelSender) UserGroupSender() *UserGroupSender {
	return &UserGroupSender{s}
}

// Send implements notification.Sender.
func (s *UserGroupSender) Send(ctx context.Context, msg notification.Message) (*notification.SentMessage, error) {
	err := permission.LimitCheckAny(ctx, permission.User, permission.System)
	if err != nil {
		return nil, err
	}

	t, ok := msg.(notification.ScheduleOnCallUsers)
	if !ok {
		return nil, errors.Errorf("unsupported message type: %T", msg)
	}

	if t.Dest.Type != notification.DestTypeSlackUG {
		return nil, errors.Errorf("unsupported destination type: %s", t.Dest.Type.String())
	}

	teamID, err := s.TeamID(ctx)
	if err != nil {
		return nil, fmt.Errorf("lookup team ID: %w", err)
	}

	var userIDs []string
	for _, u := range t.Users {
		userIDs = append(userIDs, u.ID)
	}

	userSlackIDs := make(map[string]string, len(t.Users))
	err = s.cfg.UserStore.AuthSubjectsFunc(ctx, "slack:"+teamID, userIDs, func(sub user.AuthSubject) error {
		userSlackIDs[sub.UserID] = sub.SubjectID
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("lookup user slack IDs: %w", err)
	}

	var slackUsers []string
	var missing []notification.User
	for _, u := range t.Users {
		slackID, ok := userSlackIDs[u.ID]
		if !ok {
			missing = append(missing, u)
			continue
		}

		slackUsers = append(slackUsers, slackID)
	}

	ugID, chanID, _ := strings.Cut(t.Dest.Value, ":")
	cfg := config.FromContext(ctx)

	onCallMsg := renderOnCallNotificationMessage(t, userSlackIDs)
	var stateDetails string

	// If any users are missing, we need to abort and let the channel know.
	switch {
	case len(missing) > 0:
		// TODO: add link action button to invite missing users
		var buf bytes.Buffer
		err := userGroupErrorMissing.Execute(&buf, userGroupError{
			GroupID:      ugID,
			Missing:      missing,
			callbackFunc: cfg.CallbackURL,
			OnCall:       onCallMsg,
		})
		if err != nil {
			return nil, fmt.Errorf("execute template: %w", err)
		}
		onCallMsg = buf.String()
		stateDetails = "missing users, sent error to channel"

		// If no users are on-call, we need to abort and let the channel know.
		//
		// This is because we can't update the user group with no members.
	case len(slackUsers) == 0:
		var buf bytes.Buffer
		err := userGroupErrorEmpty.Execute(&buf, userGroupError{
			GroupID:      ugID,
			ScheduleID:   t.ScheduleID,
			ScheduleName: t.ScheduleName,
			callbackFunc: cfg.CallbackURL,
			OnCall:       onCallMsg,
		})
		if err != nil {
			return nil, fmt.Errorf("execute template: %w", err)
		}
		onCallMsg = buf.String()
		stateDetails = "empty user-group, sent error to channel"
	default:
		err = s.withClient(ctx, func(c *slack.Client) error {
			_, err := c.UpdateUserGroupMembersContext(ctx, ugID, strings.Join(slackUsers, ","))
			if err != nil {
				return fmt.Errorf("update user group '%s': %w", ugID, err)
			}

			return nil
		})
	}

	// If there was an error, we need to let the channel know.
	if err != nil {
		errID := uuid.New()
		log.Log(log.WithField(ctx, "SlackUGErrorID", errID), err)
		var buf bytes.Buffer
		err := userGroupErrorUpdate.Execute(&buf, userGroupError{
			ErrorID: errID,
			GroupID: ugID,
			OnCall:  onCallMsg,
		})
		if err != nil {
			return nil, fmt.Errorf("execute template: %w", err)
		}
		onCallMsg = buf.String()
		stateDetails = "failed to update user-group, sent error to channel and log"
	}

	var ts string
	err = s.withClient(ctx, func(c *slack.Client) error {
		_, ts, err = c.PostMessageContext(ctx, chanID, slack.MsgOptionText(onCallMsg, false))
		if err != nil {
			return fmt.Errorf("post message to channel '%s': %w", chanID, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &notification.SentMessage{State: notification.StateDelivered, ExternalID: ts, StateDetails: stateDetails}, nil
}
