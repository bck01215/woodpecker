package bitbucketserver

// WARNING! This is an work-in-progress patch and does not yet conform to the coding,
// quality or security standards expected of this project. Please use with caution.

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
 	"github.com/drone/drone/remote/bitbucketserver/internal"
	"github.com/drone/drone/model"
	"github.com/drone/drone/remote"
	"github.com/mrjones/oauth"
	"strings"
	"crypto/tls"
	"encoding/hex"
	"crypto/md5"
)

const (
	requestTokenURL = "%s/plugins/servlet/oauth/request-token"
	authorizeTokenURL = "%s/plugins/servlet/oauth/authorize"
	accessTokenURL = "%s/plugins/servlet/oauth/access-token"
)



// Opts defines configuration options.
type Opts struct {
	URL         string // Stash server url.
	Username    string // Git machine account username.
	Password    string // Git machine account password.
	ConsumerKey string // Oauth1 consumer key.
	ConsumerRSA string // Oauth1 consumer key file.
	SkipVerify bool // Skip ssl verification.
}

type Config struct {
	URL string
	Username string
	Password string
	PrivateKey *rsa.PrivateKey
	ConsumerKey string
	SkipVerify bool

}

// New returns a Remote implementation that integrates with Bitbucket Server,
// the on-premise edition of Bitbucket Cloud, formerly known as Stash.
func New(opts Opts) (remote.Remote, error) {
	config := &Config{
		URL: opts.URL,
		Username: opts.Username,
		Password: opts.Password,
		ConsumerKey: opts.ConsumerKey,
		SkipVerify: opts.SkipVerify,
	}

	switch {
	case opts.Username == "":
		return nil, fmt.Errorf("Must have a git machine account username")
	case opts.Password == "":
		return nil, fmt.Errorf("Must have a git machine account password")
	case opts.ConsumerKey == "":
		return nil, fmt.Errorf("Must have a oauth1 consumer key")
	case opts.ConsumerRSA == "":
		return nil, fmt.Errorf("Must have a oauth1 consumer key file")
	}

	keyFile, err := ioutil.ReadFile(opts.ConsumerRSA)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(keyFile)
	config.PrivateKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return config, nil
}


func (c *Config) Login(res http.ResponseWriter, req *http.Request) (*model.User, error) {
	requestToken, url, err := c.Consumer().GetRequestTokenAndUrl("oob")
	if err != nil {
		return nil, err
	}
	var code = req.FormValue("oauth_verifier")
	if len(code) == 0 {
		http.Redirect(res, req, url, http.StatusSeeOther)
		return nil, nil
	}
	requestToken.Token = req.FormValue("oauth_token")
	accessToken, err := c.Consumer().AuthorizeToken(requestToken, code)
	if err != nil {
		return nil, err
	}

	client := internal.NewClientWithToken(c.URL, c.Consumer(), accessToken.Token)

	return client.FindCurrentUser()


}

// Auth is not supported by the Stash driver.
func (*Config) Auth(token, secret string) (string, error) {
	return "", fmt.Errorf("Not Implemented")
}

// Teams is not supported by the Stash driver.
func (*Config) Teams(u *model.User) ([]*model.Team, error) {
	var teams []*model.Team
	return teams, nil
}

func (c *Config) Repo(u *model.User, owner, name string) (*model.Repo, error) {

	client := internal.NewClientWithToken(c.URL, c.Consumer(), u.Token)

	return client.FindRepo(owner, name)
}

func (c *Config) Repos(u *model.User) ([]*model.RepoLite, error) {

	client := internal.NewClientWithToken(c.URL,c.Consumer(), u.Token)

	return client.FindRepos()
}

func (c *Config) Perm(u *model.User, owner, repo string) (*model.Perm, error) {
	client := internal.NewClientWithToken(c.URL,c.Consumer(), u.Token)

	return client.FindRepoPerms(owner, repo)
}

func (c *Config) File(u *model.User, r *model.Repo, b *model.Build, f string) ([]byte, error) {

	client := internal.NewClientWithToken(c.URL, c.Consumer(), u.Token)

	return client.FindFileForRepo(r.Owner, r.Name, f)
}

// Status is not supported by the Gogs driver.
func (*Config) Status(*model.User, *model.Repo, *model.Build, string) error {
	return nil
}

func (c *Config) Netrc(user *model.User, r *model.Repo) (*model.Netrc, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	//remove the port
	tmp := strings.Split(u.Host, ":")
	var host = tmp[0]

	if err != nil {
		return nil, err
	}
	return &model.Netrc{
		Machine:  host,
		Login:    c.Username,
		Password: c.Password,
	}, nil
}

func (c *Config) Activate(u *model.User, r *model.Repo, link string) error {
	client := internal.NewClientWithToken(c.URL, c.Consumer(), u.Token)

	return client.CreateHook(r.Owner, r.Name, link)
}

func (c *Config) Deactivate(u *model.User, r *model.Repo, link string) error {
	client := internal.NewClientWithToken(c.URL, c.Consumer(), u.Token)
	return client.DeleteHook(r.Owner, r.Name, link)
}

func (c *Config) Hook(r *http.Request) (*model.Repo, *model.Build, error) {
	hook := new(postHook)
	if err := json.NewDecoder(r.Body).Decode(hook); err != nil {
		return nil, nil, err
	}

	build := &model.Build{
		Event:  model.EventPush,
		Ref:    hook.RefChanges[0].RefID,                               // TODO check for index Values
		Author: hook.Changesets.Values[0].ToCommit.Author.EmailAddress, // TODO check for index Values
		Commit: hook.RefChanges[0].ToHash,                              // TODO check for index value
		Avatar: avatarLink(hook.Changesets.Values[0].ToCommit.Author.EmailAddress),
	}

	repo := &model.Repo{
		Name:     hook.Repository.Slug,
		Owner:    hook.Repository.Project.Key,
		FullName: fmt.Sprintf("%s/%s", hook.Repository.Project.Key, hook.Repository.Slug),
		Branch:   "master",
		Kind:     model.RepoGit,
	}

	return repo, build, nil
}


func (c *Config) Consumer()  *oauth.Consumer{
	consumer := oauth.NewRSAConsumer(
		c.ConsumerKey,
		c.PrivateKey,
		oauth.ServiceProvider{
			RequestTokenUrl:   fmt.Sprintf(requestTokenURL, c.URL),
			AuthorizeTokenUrl: fmt.Sprintf(authorizeTokenURL, c.URL),
			AccessTokenUrl:    fmt.Sprintf(accessTokenURL, c.URL),
			HttpMethod:        "POST",
		})
	consumer.HttpClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	return consumer
}

func avatarLink(email string) (url string) {
	hasher := md5.New()
	hasher.Write([]byte(strings.ToLower(email)))
	emailHash := fmt.Sprintf("%v", hex.EncodeToString(hasher.Sum(nil)))
	avatarURL := fmt.Sprintf("https://www.gravatar.com/avatar/%s.jpg", emailHash)
	return avatarURL
}
