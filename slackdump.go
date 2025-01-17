package slackdump

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/trace"
	"time"
	"unicode/utf8"

	"github.com/pkg/errors"
	"github.com/rusq/dlog"
	"github.com/slack-go/slack"
	"golang.org/x/time/rate"

	"github.com/rusq/slackdump/v2/auth"
	"github.com/rusq/slackdump/v2/internal/network"
)

//go:generate mockgen -destination internal/mocks/mock_os/mock_os.go os FileInfo
//go:generate mockgen -destination internal/mocks/mock_downloader/mock_downloader.go github.com/rusq/slackdump/v2/downloader Downloader
//go:generate sh -c "mockgen -source slackdump.go -destination clienter_mock.go -package slackdump -mock_names clienter=mockClienter,Reporter=mockReporter"
//go:generate sed -i ~ -e "s/NewmockClienter/newmockClienter/g" -e "s/NewmockReporter/newmockReporter/g" clienter_mock.go

// SlackDumper stores basic session parameters.
type SlackDumper struct {
	client clienter

	teamID string // used as a suffix for cached users

	// Users contains the list of users and populated on NewSlackDumper
	Users     Users                  `json:"users"`
	UserIndex map[string]*slack.User `json:"-"`

	options Options
}

// clienter is the interface with some functions of slack.Client with the sole
// purpose of mocking in tests (see client_mock.go)
type clienter interface {
	GetConversationInfoContext(ctx context.Context, channelID string, includeLocale bool) (*slack.Channel, error)
	GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) (msgs []slack.Message, hasMore bool, nextCursor string, err error)
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) (channels []slack.Channel, nextCursor string, err error)
	GetFile(downloadURL string, writer io.Writer) error
	GetTeamInfo() (*slack.TeamInfo, error)
	GetUsersContext(ctx context.Context) ([]slack.User, error)
}

// AllChanTypes enumerates all API-supported channel types as of 03/2022.
var AllChanTypes = []string{"mpim", "im", "public_channel", "private_channel"}

// Reporter is an interface defining output functions
type Reporter interface {
	ToText(w io.Writer, sd *SlackDumper) error
}

// New creates new client and populates the internal cache of users and channels
// for lookups.
func New(ctx context.Context, creds auth.Provider, opts ...Option) (*SlackDumper, error) {
	options := DefOptions
	for _, opt := range opts {
		opt(&options)
	}

	return NewWithOptions(ctx, creds, options)
}

func (sd *SlackDumper) Client() *slack.Client {
	return sd.client.(*slack.Client)
}

func NewWithOptions(ctx context.Context, authProvider auth.Provider, opts Options) (*SlackDumper, error) {
	ctx, task := trace.NewTask(ctx, "NewWithOptions")
	defer task.End()

	if err := authProvider.Validate(); err != nil {
		return nil, err
	}

	cl := slack.New(authProvider.SlackToken(), slack.OptionCookieRAW(toPtrCookies(authProvider.Cookies())...))
	ti, err := cl.GetTeamInfoContext(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	sd := &SlackDumper{
		client:  cl,
		options: opts,
		teamID:  ti.ID,
	}

	dlog.Println("> checking user cache...")
	users, err := sd.GetUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("error fetching users: %w", err)
	}

	sd.Users = users
	sd.UserIndex = users.IndexByID()

	return sd, nil
}

func toPtrCookies(cc []http.Cookie) []*http.Cookie {
	var ret = make([]*http.Cookie, len(cc))
	for i := range cc {
		ret[i] = &cc[i]
	}
	return ret
}

func (sd *SlackDumper) limiter(t network.Tier) *rate.Limiter {
	return network.NewLimiter(t, sd.options.Tier3Burst, int(sd.options.Tier3Boost))
}

// withRetry will run the callback function fn. If the function returns
// slack.RateLimitedError, it will delay, and then call it again up to
// maxAttempts times. It will return an error if it runs out of attempts.
func withRetry(ctx context.Context, l *rate.Limiter, maxAttempts int, fn func() error) error {
	return network.WithRetry(ctx, l, maxAttempts, fn)
}

func maxStringLength(strings []string) (maxlen int) {
	for i := range strings {
		l := utf8.RuneCountInString(strings[i])
		if l > maxlen {
			maxlen = l
		}
	}
	return
}

func checkCacheFile(filename string, maxAge time.Duration) error {
	if filename == "" {
		return errors.New("no cache filename")
	}
	fi, err := os.Stat(filename)
	if err != nil {
		return err
	}

	return validateFileStats(fi, maxAge)
}

func validateFileStats(fi os.FileInfo, maxAge time.Duration) error {
	if fi.IsDir() {
		return errors.New("cache file is a directory")
	}
	if fi.Size() == 0 {
		return errors.New("empty cache file")
	}
	if time.Since(fi.ModTime()) > maxAge {
		return errors.New("cache expired")
	}
	return nil
}
