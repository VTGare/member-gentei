package discord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog/log"
)

type roleAction string

func (r roleAction) String() string {
	return string(r)
}

const (
	roleAdd    roleAction = "Grant role"
	roleRevoke roleAction = "Revoke role"

	defaultRoleApplyPeriod  = time.Second * 5
	defaultRoleApplyTimeout = time.Second * 30
)

// sometimes Discord has a hernia and we have to keep asking it to apply a membership role change.
// This manages a series of retry loops that rely on a GUILD_MEMBER_UPDATE event to interrupt it.
func (d *discordBot) newRoleApplier(
	guildID string,
	user *discordgo.User,
	action roleAction,
	reason string,
	tries int,
	period time.Duration,
	timeout time.Duration,
) {
	// * derive a context with the timeout - retryCtx
	// * start a ticker for the retry that applies the role and cleans up when successful
	// * start a listener for when an applicable discordgo.GuildMemberUpdate comes in to updateChan, and cancel retryContext
	var (
		updateChan               = make(chan *discordgo.GuildMemberUpdate, 1)
		updateKey                = fmt.Sprintf("%s-%s", guildID, user.ID)
		retryCtx, cancelRetryCtx = context.WithDeadline(d.ctx, time.Now().Add(timeout))
		// retryCount is 1-indexed
		retryCount = 1
		// if the role ID changes we flush all appliers when they come around
		initialRoleID = d.guildStates[guildID].Doc.MemberRoleID

		// not state, just easy
		userID = user.ID
	)
	log.Info().
		Str("guildID", guildID).Str("userID", user.ID).
		Str("action", action.String()).
		Msg("starting roleApplier")
	// if a check is already in progress, log that we're ignoring this attempt
	if _, exists := d.guildMemberUpdateChannels.Load(updateKey); exists {
		log.Info().Str("guildID", guildID).Str("userID", userID).Msg("ignoring request, pending change already in progress")
	}
	d.guildMemberUpdateChannels.Store(updateKey, updateChan)
	// applies the role change
	applyRoleFunc := func() {
		// check if the role ID has changed, and if not - apply the change
		var (
			guildState = d.guildStates[guildID]
			logger     = log.With().
					Str("guildID", guildID).Str("roleID", guildState.Doc.MemberRoleID).
					Str("userID", userID).
					Str("reason", reason).
					Int("retry", retryCount).Logger()
		)
		if retryCount >= tries {
			logger.Error().Msg("reached max retries, terminating role applier")
			cancelRetryCtx()
			return
		}
		if guildState.Doc.MemberRoleID != initialRoleID {
			logger.Info().Str("oldRoleID", initialRoleID).Msg("role ID has changed, terminating role applier")
			cancelRetryCtx()
			return
		}
		retryCount++
		switch action {
		case roleAdd:
			logger.Info().Msg("attempting to add role")
			err := d.grantMemberRole(guildID, user, reason)
			if err != nil {
				if strings.Contains(err.Error(), "HTTP 403 Forbidden") {
					logger.Err(err).Msg("403 adding user to member role, cancelling retries")
					cancelRetryCtx()
				} else {
					logger.Err(err).Msg("error performing API call to add user to member role")
				}
				return
			}
		case roleRevoke:
			logger.Info().Msg("attempting to revoke role")
			err := d.revokeMemberRole(guildID, user, reason)
			if err != nil {
				if strings.Contains(err.Error(), "HTTP 403 Forbidden") {
					logger.Err(err).Msg("403 removing user from member role, cancelling retries")
					cancelRetryCtx()
				} else {
					logger.Err(err).Msg("error performing API call to remove user to member role")
				}
			}
		}
	}
	// reacts to membership updates
	memberUpdateFunc := func(gmu *discordgo.GuildMemberUpdate) {
		var (
			guildState   = d.guildStates[guildID]
			targetRoleID = guildState.Doc.MemberRoleID
			logger       = log.With().
					Str("guildID", guildID).Str("roleID", targetRoleID).
					Str("userID", userID).
					Str("reason", reason).
					Int("retry", retryCount-1).Logger() // subtract because we log "succeeded after retry #n"
			anyMatch bool
		)
		for _, role := range gmu.Member.Roles {
			if role == targetRoleID {
				anyMatch = true
				break
			}
		}
		// if add/remove succeeded
		if (action == roleAdd && anyMatch) || (action == roleRevoke && !anyMatch) {
			switch action {
			case roleAdd:
				logger.Info().Msg("added user to members-only role")
			case roleRevoke:
				logger.Info().Msg("removed user from members-only role")
			}
			if auditChannelID := guildState.Doc.AuditLogChannelID; auditChannelID != "" {
				d.emitMemberAuditLog(auditChannelID, action, userID, user.AvatarURL(""), reason)
			}
		}
		// success!
		cancelRetryCtx()
	}
	// the ticker
	go func() {
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		// do it once now, before the ticker starts
		applyRoleFunc()
		for {
			select {
			case <-retryCtx.Done():
				// it's over!
				close(updateChan)
				d.guildMemberUpdateChannels.Delete(updateKey)
				return
			case <-ticker.C:
				// retry until the context is dead or we have a roleUpdate
				applyRoleFunc()
			case roleUpdate := <-updateChan:
				memberUpdateFunc(roleUpdate)
			}
		}
	}()
}

func (d *discordBot) grantMemberRole(guildID string, user *discordgo.User, reason string) error {
	var (
		guildState = d.guildStates[guildID]
		userID     = user.ID
	)
	err := d.dgSession.GuildMemberRoleAdd(guildID, userID, guildState.Doc.MemberRoleID)
	if err != nil {
		return err
	}
	return nil
}

func (d *discordBot) revokeMemberRole(guildID string, user *discordgo.User, reason string) error {
	var (
		guildState = d.guildStates[guildID]
		userID     = user.ID
	)
	err := d.dgSession.GuildMemberRoleRemove(guildID, userID, guildState.Doc.MemberRoleID)
	if err != nil {
		return err
	}
	return nil
}
