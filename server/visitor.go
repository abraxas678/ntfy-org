package server

import (
	"errors"
	"heckel.io/ntfy/user"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"heckel.io/ntfy/util"
)

const (
	// visitorExpungeAfter defines how long a visitor is active before it is removed from memory. This number
	// has to be very high to prevent e-mail abuse, but it doesn't really affect the other limits anyway, since
	// they are replenished faster (typically).
	visitorExpungeAfter = 24 * time.Hour
)

var (
	errVisitorLimitReached = errors.New("limit reached")
)

// visitor represents an API user, and its associated rate.Limiter used for rate limiting
type visitor struct {
	config              *Config
	messageCache        *messageCache
	userManager         *user.Manager // May be nil!
	ip                  netip.Addr
	user                *user.User
	messages            int64         // Number of messages sent, reset every day
	emails              int64         // Number of emails sent, reset every day
	requestLimiter      *rate.Limiter // Rate limiter for (almost) all requests (including messages)
	messagesLimiter     util.Limiter  // Rate limiter for messages, may be nil
	emailsLimiter       *rate.Limiter // Rate limiter for emails
	subscriptionLimiter util.Limiter  // Fixed limiter for active subscriptions (ongoing connections)
	bandwidthLimiter    util.Limiter  // Limiter for attachment bandwidth downloads
	accountLimiter      *rate.Limiter // Rate limiter for account creation
	firebase            time.Time     // Next allowed Firebase message
	seen                time.Time
	mu                  sync.Mutex
}

type visitorInfo struct {
	Basis                        string // "ip", "role" or "tier"
	Messages                     int64
	MessagesLimit                int64
	MessagesRemaining            int64
	MessagesExpiryDuration       int64
	Emails                       int64
	EmailsLimit                  int64
	EmailsRemaining              int64
	Reservations                 int64
	ReservationsLimit            int64
	ReservationsRemaining        int64
	AttachmentTotalSize          int64
	AttachmentTotalSizeLimit     int64
	AttachmentTotalSizeRemaining int64
	AttachmentFileSizeLimit      int64
	AttachmentExpiryDuration     int64
}

func newVisitor(conf *Config, messageCache *messageCache, userManager *user.Manager, ip netip.Addr, user *user.User) *visitor {
	var messagesLimiter util.Limiter
	var requestLimiter, emailsLimiter, accountLimiter *rate.Limiter
	var messages, emails int64
	if user != nil {
		messages = user.Stats.Messages
		emails = user.Stats.Emails
	} else {
		accountLimiter = rate.NewLimiter(rate.Every(conf.VisitorAccountCreateLimitReplenish), conf.VisitorAccountCreateLimitBurst)
	}
	if user != nil && user.Tier != nil {
		requestLimiter = rate.NewLimiter(dailyLimitToRate(user.Tier.MessagesLimit), conf.VisitorRequestLimitBurst)
		messagesLimiter = util.NewFixedLimiter(user.Tier.MessagesLimit)
		emailsLimiter = rate.NewLimiter(dailyLimitToRate(user.Tier.EmailsLimit), conf.VisitorEmailLimitBurst)
	} else {
		requestLimiter = rate.NewLimiter(rate.Every(conf.VisitorRequestLimitReplenish), conf.VisitorRequestLimitBurst)
		emailsLimiter = rate.NewLimiter(rate.Every(conf.VisitorEmailLimitReplenish), conf.VisitorEmailLimitBurst)
	}
	return &visitor{
		config:              conf,
		messageCache:        messageCache,
		userManager:         userManager, // May be nil!
		ip:                  ip,
		user:                user,
		messages:            messages,
		emails:              emails,
		requestLimiter:      requestLimiter,
		messagesLimiter:     messagesLimiter,
		emailsLimiter:       emailsLimiter,
		subscriptionLimiter: util.NewFixedLimiter(int64(conf.VisitorSubscriptionLimit)),
		bandwidthLimiter:    util.NewBytesLimiter(conf.VisitorAttachmentDailyBandwidthLimit, 24*time.Hour),
		accountLimiter:      accountLimiter, // May be nil
		firebase:            time.Unix(0, 0),
		seen:                time.Now(),
	}
}

func (v *visitor) RequestAllowed() error {
	if !v.requestLimiter.Allow() {
		return errVisitorLimitReached
	}
	return nil
}

func (v *visitor) FirebaseAllowed() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Now().Before(v.firebase) {
		return errVisitorLimitReached
	}
	return nil
}

func (v *visitor) FirebaseTemporarilyDeny() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.firebase = time.Now().Add(v.config.FirebaseQuotaExceededPenaltyDuration)
}

func (v *visitor) MessageAllowed() error {
	if v.messagesLimiter != nil && v.messagesLimiter.Allow(1) != nil {
		return errVisitorLimitReached
	}
	return nil
}

func (v *visitor) EmailAllowed() error {
	if !v.emailsLimiter.Allow() {
		return errVisitorLimitReached
	}
	return nil
}

func (v *visitor) SubscriptionAllowed() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.subscriptionLimiter.Allow(1); err != nil {
		return errVisitorLimitReached
	}
	return nil
}

func (v *visitor) RemoveSubscription() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.subscriptionLimiter.Allow(-1)
}

func (v *visitor) Keepalive() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = time.Now()
}

func (v *visitor) BandwidthLimiter() util.Limiter {
	return v.bandwidthLimiter
}

func (v *visitor) Stale() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return time.Since(v.seen) > visitorExpungeAfter
}

func (v *visitor) IncrMessages() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.messages++
	if v.user != nil {
		v.user.Stats.Messages = v.messages
	}
}

func (v *visitor) IncrEmails() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.emails++
	if v.user != nil {
		v.user.Stats.Emails = v.emails
	}
}

func (v *visitor) Info() (*visitorInfo, error) {
	v.mu.Lock()
	messages := v.messages
	emails := v.emails
	v.mu.Unlock()
	info := &visitorInfo{}
	if v.user != nil && v.user.Role == user.RoleAdmin {
		info.Basis = "role"
		// All limits are zero!
		info.MessagesExpiryDuration = 24 * 3600   // FIXME this is awful. Should be from the Unlimited plan
		info.AttachmentExpiryDuration = 24 * 3600 // FIXME this is awful. Should be from the Unlimited plan
	} else if v.user != nil && v.user.Tier != nil {
		info.Basis = "tier"
		info.MessagesLimit = v.user.Tier.MessagesLimit
		info.MessagesExpiryDuration = v.user.Tier.MessagesExpiryDuration
		info.EmailsLimit = v.user.Tier.EmailsLimit
		info.ReservationsLimit = v.user.Tier.ReservationsLimit
		info.AttachmentTotalSizeLimit = v.user.Tier.AttachmentTotalSizeLimit
		info.AttachmentFileSizeLimit = v.user.Tier.AttachmentFileSizeLimit
		info.AttachmentExpiryDuration = v.user.Tier.AttachmentExpiryDuration
	} else {
		info.Basis = "ip"
		info.MessagesLimit = replenishDurationToDailyLimit(v.config.VisitorRequestLimitReplenish)
		info.MessagesExpiryDuration = int64(v.config.CacheDuration.Seconds())
		info.EmailsLimit = replenishDurationToDailyLimit(v.config.VisitorEmailLimitReplenish)
		info.ReservationsLimit = 0 // FIXME
		info.AttachmentTotalSizeLimit = v.config.VisitorAttachmentTotalSizeLimit
		info.AttachmentFileSizeLimit = v.config.AttachmentFileSizeLimit
		info.AttachmentExpiryDuration = int64(v.config.AttachmentExpiryDuration.Seconds())
	}
	var attachmentsBytesUsed int64 // FIXME Maybe move this to endpoint?
	var err error
	if v.user != nil {
		attachmentsBytesUsed, err = v.messageCache.AttachmentBytesUsedByUser(v.user.Name)
	} else {
		attachmentsBytesUsed, err = v.messageCache.AttachmentBytesUsedBySender(v.ip.String())
	}
	if err != nil {
		return nil, err
	}
	var reservations int64
	if v.user != nil && v.userManager != nil {
		reservations, err = v.userManager.ReservationsCount(v.user.Name) // FIXME dup call, move this to endpoint?
		if err != nil {
			return nil, err
		}
	}
	info.Messages = messages
	info.MessagesRemaining = zeroIfNegative(info.MessagesLimit - info.Messages)
	info.Emails = emails
	info.EmailsRemaining = zeroIfNegative(info.EmailsLimit - info.Emails)
	info.Reservations = reservations
	info.ReservationsRemaining = zeroIfNegative(info.ReservationsLimit - info.Reservations)
	info.AttachmentTotalSize = attachmentsBytesUsed
	info.AttachmentTotalSizeRemaining = zeroIfNegative(info.AttachmentTotalSizeLimit - info.AttachmentTotalSize)
	return info, nil
}

func zeroIfNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func replenishDurationToDailyLimit(duration time.Duration) int64 {
	return int64(24 * time.Hour / duration)
}

func dailyLimitToRate(limit int64) rate.Limit {
	return rate.Limit(limit) * rate.Every(24*time.Hour)
}
