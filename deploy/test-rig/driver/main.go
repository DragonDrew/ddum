//go:build rig

// Command driver plays the Discord user for the end-to-end rig . It talks to a bot running with DISCORD_MODE=fake through the signed
// /test/v1 endpoints.
//
//	driver approve-host            wait for the registration notice and click Approve
//	driver cmd <command> <sub> [option=value...]
//	driver deploy <stack>          /update check, click Approve on the stack's message,
//	                               wait for "Deployed"
//	driver schedule <scope>        /update check, open the scope from the result
//	                               card's menu, Schedule it, check it waits, then
//	                               bring it forward and wait for "Deployed"
//	driver messages                print every message the bot has sent
//
// Like a person in the notification channel, it clicks buttons on the cards
// themselves, and cards change in place: a stack's card becomes its deploy's
// card, then its result.
//
// Environment: RIG_BOT_URL, RIG_KEY (the agent key), RIG_GUILD, RIG_CHANNEL, RIG_USER.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/discord"
	"github.com/DragonDrew/ddum/internal/protocol"
)

const driverID = "0195f2c4-0000-7000-8000-0000000d71e7"

var (
	botURL  = env("RIG_BOT_URL", "http://127.0.0.1:18080")
	key     = []byte(os.Getenv("RIG_KEY"))
	guild   = env("RIG_GUILD", "100000000000000001")
	channel = env("RIG_CHANNEL", "100000000000000003")
	user    = env("RIG_USER", "100000000000000002")
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func post(path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, botURL+path, bytes.NewReader(b))
	protocol.SignHTTPRequest(req, key, driverID, b, time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %d %s", path, resp.StatusCode, data)
	}
	return json.Unmarshal(data, out)
}

func interact(it discord.Interaction) discord.TestResponse {
	it.GuildID, it.ChannelID, it.UserID, it.UserName = guild, channel, user, "rig"
	var r discord.TestResponse
	if err := post("/test/v1/interaction", it, &r); err != nil {
		die("interaction: %v", err)
	}
	return r
}

// messages returns what the bot has sent since it started. While the bot
// restarts (self-update) it retries for up to two minutes.
func messages() []discord.TestMessage {
	var m []discord.TestMessage
	var err error
	for i := 0; i < 120; i++ {
		if err = post("/test/v1/messages", struct{}{}, &m); err == nil {
			return m
		}
		time.Sleep(time.Second)
	}
	die("messages: %v", err)
	return nil
}

// isResult reports whether m is an operation result for stack.
func isResult(m discord.TestMessage, stack string) bool {
	return strings.Contains(m.Text, "· "+stack) && strings.Contains(m.Text, "details: /history show")
}

// resultFor waits for a result for stack that is new or changed since seen
// (cards are edited in place) and returns it.
func resultFor(stack string, seen map[string]string, timeout time.Duration) discord.TestMessage {
	return waitFor("the result for "+stack, timeout, func(m discord.TestMessage) bool {
		if t, ok := seen[m.MessageID]; ok && t == m.Text {
			return false
		}
		return isResult(m, stack)
	})
}

// seenNow is every message's current text.
func seenNow() map[string]string {
	seen := map[string]string{}
	for _, m := range messages() {
		seen[m.MessageID] = m.Text
	}
	return seen
}

// click presses a button on a card.
func click(messageID, id string) discord.TestResponse {
	if id == "" {
		die("no such button on %s", messageID)
	}
	return interact(discord.Interaction{Kind: discord.KindComponent, CustomID: id, MessageID: messageID})
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "driver: "+format+"\n", a...)
	os.Exit(1)
}

// waitFor polls the sent messages until match returns true, newest first: a
// stack can have an older card still on show, offering only what it found then.
func waitFor(what string, timeout time.Duration, match func(discord.TestMessage) bool) discord.TestMessage {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ms := messages()
		for i := len(ms) - 1; i >= 0; i-- {
			if match(ms[i]) {
				return ms[i]
			}
		}
		time.Sleep(time.Second)
	}
	for _, m := range messages() {
		fmt.Fprintln(os.Stderr, "---", m.MessageID, "\n"+m.Text)
	}
	die("timed out after %s waiting for %s", timeout, what)
	return discord.TestMessage{}
}

func button(m discord.TestMessage, prefix string) string {
	for _, id := range m.CustomIDs {
		if strings.HasPrefix(id, prefix) {
			return id
		}
	}
	return ""
}

// check runs /update check, waiting out a scheduled check that is already
// running: it may have listed the tags before the rig pushed new images.
func check() discord.TestResponse {
	for i := 0; ; i++ {
		r := interact(discord.Interaction{Kind: discord.KindCommand, Command: "update", Subcommand: "check"})
		if !strings.Contains(r.Text, "already running") {
			return r
		}
		if i == 180 {
			die("a registry check has been running for 3 minutes: %s", r.Text)
		}
		time.Sleep(time.Second)
	}
}

// checkDone runs /update check and waits for its own card to show the result.
// On a slow host a scheduled check can post an update card while images are still
// being pushed, and clicking that card would leave the later ones out.
func checkDone() discord.TestMessage {
	r := check()
	return waitFor("the check to finish", 3*time.Minute, func(m discord.TestMessage) bool {
		return m.MessageID == r.MessageID && (strings.Contains(m.Text, "Check finished") || strings.Contains(m.Text, "Up to date"))
	})
}

func main() {
	if len(os.Args) < 2 {
		die("usage: driver approve-host | cmd <command> <sub> [k=v...] | deploy <stack> | messages")
	}
	switch os.Args[1] {
	case "approve-host":
		m := waitFor("a host registration notice", 2*time.Minute, func(m discord.TestMessage) bool {
			return button(m, "v1:host_approve:") != ""
		})
		r := click(m.MessageID, button(m, "v1:host_approve:"))
		fmt.Println(r.Text)
	case "cmd":
		if len(os.Args) < 4 {
			die("usage: driver cmd <command> <sub> [k=v...]")
		}
		opts := map[string]string{}
		for _, kv := range os.Args[4:] {
			k, v, _ := strings.Cut(kv, "=")
			opts[k] = v
		}
		r := interact(discord.Interaction{Kind: discord.KindCommand, Command: os.Args[2], Subcommand: os.Args[3], Options: opts})
		fmt.Println(r.Text)
	case "deploy":
		if len(os.Args) < 3 {
			die("usage: driver deploy <stack>")
		}
		stack := os.Args[2]
		fmt.Println(checkDone().Text)
		m := waitFor("an update message for "+stack+" with Approve", 3*time.Minute, func(m discord.TestMessage) bool {
			return strings.Contains(m.Text, "Updates for "+stack) && button(m, "v1:approve:") != ""
		})
		fmt.Println("update message:\n" + m.Text)
		seen := seenNow()
		r := click(m.MessageID, button(m, "v1:approve:"))
		fmt.Println("approve:\n" + r.Text)
		if !strings.Contains(r.Text, "queued") || r.MessageID != m.MessageID {
			die("approval failed: the card should turn into the queued deploy")
		}
		res := resultFor(stack, seen, 5*time.Minute)
		if res.MessageID != m.MessageID {
			die("the result should replace the approved card %s, not land on %s", m.MessageID, res.MessageID)
		}
		fmt.Println("result:\n" + res.Text)
		if !strings.Contains(res.Text, "Deployed · "+stack) {
			die("deployment did not succeed")
		}
	case "deploy-refused":
		// deploy-refused <stack> <text>: approving must end in a failed deploy
		// whose reason contains text.
		stack, want := os.Args[2], os.Args[3]
		checkDone()
		m := waitFor("an update message for "+stack+" with Approve", 3*time.Minute, func(m discord.TestMessage) bool {
			return strings.Contains(m.Text, "Updates for "+stack) && button(m, "v1:approve:") != ""
		})
		seen := seenNow()
		click(m.MessageID, button(m, "v1:approve:"))
		res := resultFor(stack, seen, 5*time.Minute)
		fmt.Println("result:\n" + res.Text)
		if !strings.Contains(res.Text, "Deploy failed · "+stack) || !strings.Contains(res.Text, want) {
			die("want a failed deploy mentioning %q", want)
		}
	case "rollback":
		// Roll back the stack's latest deployment from its result message.
		stack := os.Args[2]
		var id, card string
		for _, m := range messages() {
			if isResult(m, stack) {
				if b := button(m, "v1:rollback:"); b != "" {
					id, card = b, m.MessageID
				}
			}
		}
		if id == "" {
			die("no Roll back button for %s", stack)
		}
		r := click(card, id)
		fmt.Println("confirm:\n" + r.Text)
		var ok string
		for _, c := range r.CustomIDs {
			if strings.HasPrefix(c, "v1:rollback_ok:") {
				ok = c
			}
		}
		if ok == "" {
			die("rollback refused: %s", r.Text)
		}
		seen := seenNow()
		fmt.Println(click(card, ok).Text)
		res := resultFor(stack, seen, 5*time.Minute)
		fmt.Println("result:\n" + res.Text)
		if !strings.Contains(res.Text, "Rolled back · "+stack) {
			die("rollback did not succeed")
		}
	case "selfupdate":
		// selfupdate <stack> <expect: succeeded|rolledback>
		stack, expect := os.Args[2], os.Args[3]
		seen := seenNow()
		fmt.Println(check().Text)
		m := waitFor("a bot update message", 3*time.Minute, func(m discord.TestMessage) bool {
			_, old := seen[m.MessageID]
			return !old && strings.Contains(m.Text, "ddum bot update available") && button(m, "v1:selfupdate:") != ""
		})
		fmt.Println("bot update message:\n" + m.Text)
		r := click(m.MessageID, button(m, "v1:selfupdate:"))
		if !strings.Contains(r.Text, "This restarts the bot") {
			die("no restart warning: %s", r.Text)
		}
		var approve string
		for _, c := range r.CustomIDs {
			if strings.HasPrefix(c, "v1:approve:") {
				approve = c
			}
		}
		seen = seenNow()
		fmt.Println("approve:\n" + click(m.MessageID, approve).Text)
		res := resultFor(stack, seen, 6*time.Minute)
		fmt.Println("result (posted after the bot restarted):\n" + res.Text)
		want := "Bot updated · " + stack
		if expect == "rolledback" {
			want = "Bot update failed, previous version restored · " + stack
		}
		if !strings.Contains(res.Text, want) {
			die("want %q", want)
		}
	case "schedule":
		scope := os.Args[2]
		r := check()
		card := r.MessageID
		if card == "" {
			die("/update check gave no card: %s", r.Text)
		}
		m := waitFor("the check result", 3*time.Minute, func(m discord.TestMessage) bool {
			return m.MessageID == card && strings.Contains(m.Text, "Check finished")
		})
		fmt.Println("check result:\n" + m.Text)
		value, ok := m.Options[scope]
		if !ok {
			die("%s is not in the result card's menu: %v", scope, m.Options)
		}
		r = interact(discord.Interaction{Kind: discord.KindComponent, CustomID: button(m, "v1:review_pick"), MessageID: card, Values: []string{value}})
		if !strings.Contains(r.Text, "Updates for "+scope) || r.MessageID != card {
			die("opening %s from the menu: %s", scope, r.Text)
		}
		sched := ""
		for _, c := range r.CustomIDs {
			if strings.HasPrefix(c, "v1:schedule:") {
				sched = c
			}
		}
		r = click(card, sched)
		if len(r.Values) == 0 {
			die("no times offered: %s", r.Text)
		}
		pick := ""
		for _, c := range r.CustomIDs {
			if strings.HasPrefix(c, "v1:sched_pick:") {
				pick = c
			}
		}
		r = interact(discord.Interaction{Kind: discord.KindComponent, CustomID: pick, MessageID: card, Values: []string{r.Values[0]}})
		fmt.Println("scheduled:\n" + r.Text)
		if !strings.Contains(r.Text, "Scheduled · "+scope) || r.MessageID != card {
			die("the card should turn into the scheduled deploy")
		}
		// The agent polls every few seconds; nothing may start early.
		time.Sleep(20 * time.Second)
		for _, m := range messages() {
			if m.MessageID == card && !strings.Contains(m.Text, "Scheduled · "+scope) {
				die("the scheduled deploy did not wait:\n%s", m.Text)
			}
		}
		fmt.Println("ok: nothing deployed before the scheduled time")
		seen := seenNow()
		var started map[string]int64
		if err := post("/test/v1/scheduled/start", struct{}{}, &started); err != nil || started["started"] != 1 {
			die("bring forward: %v %v", started, err)
		}
		res := resultFor(scope, seen, 5*time.Minute)
		fmt.Println("result:\n" + res.Text)
		if res.MessageID != card || !strings.Contains(res.Text, "Deployed · "+scope) {
			die("the scheduled deploy's result should land on its card")
		}
	case "offer":
		// offer <stack>: /update check, wait for the stack's card, print the
		// card and its Approve button for "stale".
		stack := os.Args[2]
		checkDone()
		m := waitFor("an update message for "+stack+" with Approve", 3*time.Minute, func(m discord.TestMessage) bool {
			return strings.Contains(m.Text, "Updates for "+stack) && button(m, "v1:approve:") != ""
		})
		fmt.Println(m.MessageID, button(m, "v1:approve:"))
	case "stale":
		// stale <stack> <card> <approve id>: without any check, the card must
		// stop offering the update, and its old button must deploy nothing.
		stack, card, approve := os.Args[2], os.Args[3], os.Args[4]
		m := waitFor("the card of "+stack+" to clear", time.Minute, func(m discord.TestMessage) bool {
			return m.MessageID == card && strings.Contains(m.Text, "Nothing pending")
		})
		fmt.Println("card:\n" + m.Text)
		r := click(card, approve)
		fmt.Println("the old Approve button:\n" + r.Text)
		if strings.Contains(r.Text, "queued") {
			die("a stale card deployed something")
		}
	case "messages":
		for _, m := range messages() {
			fmt.Println("---", m.MessageID, m.CustomIDs, "\n"+m.Text)
		}
	default:
		die("unknown command %q", os.Args[1])
	}
}
