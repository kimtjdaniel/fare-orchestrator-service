// Package orchestrator is the brain. Every incoming group message lands in Brain.Handle().
//
// Flow (code moves the state, Gemini only reads/writes text). Each WhatsApp group has a SINGLETON
// trip document (no history of past trips) that gets reset and reused for every new cycle:
//
//	COLLECTING --@mention--> extract prefs + propose options --> AWAITING_CHOICE
//	AWAITING_CHOICE --"2"--> lock destination, send full plan summary, ask to finalize
//	                --yes--> search flights + hotel (live dashboard link) --> AWAITING_APPROVAL
//	AWAITING_APPROVAL --✅--> book flights + hotel (Skyvern) --> BOOKED
//	                  --❌--> back to AWAITING_CHOICE
//	ANY STATE --explicit new trip request--> fresh dashboard session, reset doc --> COLLECTING
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"fare-brain/config"
	"fare-brain/dashboard"
	"fare-brain/formatting"
	"fare-brain/llm"
	"fare-brain/messaging"
	"fare-brain/models"
	"fare-brain/prompts"
	"fare-brain/state"
	"fare-brain/store"
	"fare-brain/tools"
)

const defaultLeadEmail = "demo@fare.travel"

// Fast paths: obvious replies are handled without an LLM call (faster + free).
var (
	choiceOnlyRe  = regexp.MustCompile(`(?i)^\s*(?:option\s*)?([1-3])\s*[.!]?\s*$`)
	approveOnlyRe = regexp.MustCompile(`(?i)^\s*(✅|👍|yes|yep|book it|approve)\s*!*\s*$`)
	bookAskRe     = regexp.MustCompile(`(?i)\bbook(?:ing)?\b`)
	confirmPlanRe = regexp.MustCompile(`(?i)\b(plan(?:'s| is) good|looks good|sounds good|love (?:it|this|the plan)|let'?s (?:go|do (?:it|this)|book)|go (?:with|for) (?:that|this)|confirmed|confirm(?: the)? plan|initialize (?:the )?book|start (?:the )?(?:search|booking)|lock (?:this|it) in)\b`)
	searchAskRe   = regexp.MustCompile(`(?i)\b((?:let'?s|lets|can we|please|go ahead(?: and)?|start|ready to|time to|we should)\s+(?:search|look up|find|scout)|search(?:ing)?(?:\s+\w+){0,5}\s*(?:flight|hotel|stay|fare)|how about (?:the )?flights?|lock(?:ing)? in (?:these |the |those )?(?:specific )?(?:property|flight|hotel|option))`)
	newTripAskRe  = regexp.MustCompile(`(?i)\b(?:(?:plan|organize)\s+(?:a|an|another|new|separate)\s+(?:[\p{L}\p{N}-]+\s+){0,6}(?:trip|vacation|holiday)|(?:start|create|plan)\s+(?:a\s+)?(?:new|another|separate)\s+(?:trip|vacation|holiday|(?:planning\s+)?session)|(?:start|create)\s+(?:a|an)\s+(?:trip|vacation|holiday|(?:planning\s+)?session))\b`)
	rejectOnlyRe  = regexp.MustCompile(`(?i)^\s*(❌|👎|no|nope)\s*!*\s*$`)
	// One person stating facts about another (or about "he/she") — origin, dates, budget.
	proxyPrefRe      = regexp.MustCompile(`(?i)(flying from|flies from|leaving from|leave from|not available|i know \w+'?s|\b(he|she|they)'s (flying|not|busy)|\b(his|her|their) (schedule|dates|flight))`)
	prefFactRe       = regexp.MustCompile(`(?i)(available|can'?t|cannot|busy|flying|schedule|dates|from )`)
	itineraryAskRe   = regexp.MustCompile(`(?i)(itinerar|inten[ae]ry|itinerary|day[- ]?by[- ]?day|day[- ]?to[- ]?day|each day|every day|detailed (?:\w+\s+){0,3}plan|plan (?:for )?each day|things to do|what to visit|go visit|full \d+\s*-?\s*days?|day\s*\d+|neighbourhood|neighborhood|hidden gem)`)
	restaurantAskRe  = regexp.MustCompile(`(?i)\b(restaurants?|where to eat|places to eat|dinner spots?|food recs?|what (should|can|do) we eat|best (pizza|pasta|eats)|wine bars?|trattoria|where (?:are|should) we (?:eat|dine))\b`)
	restaurantListRe = regexp.MustCompile(`(?i)\b(top\s*\d+\s*restaurants?|list(?:\s+\w+){0,8}\s+restaurants?|restaurants?\s+by\s+(?:stars?|rating|maps)|best\s+restaurants?)\b`)
	introAskRe       = regexp.MustCompile(`(?i)\b(introduce yourself|intro yourself|who are you|what (can|do) you do|what are you capable of|your capabilities|what can fare do)\b`)
	greetingOnlyRe   = regexp.MustCompile(`(?i)^(?:@\S+\s+)*(?:hi|hey|hello|yo|sup|what'?s up|help|you there)?[\s!.,?]*$`)
	atTokenRe        = regexp.MustCompile(`(?i)@\S+`)
	hotelAskRe       = regexp.MustCompile(`(?i)(\bhotels?\b|\bthe stay\b|where (?:are|we'?re|will) we stay|\baccommodat|\bthe room\b|show (?:me |us )?(?:the )?(?:hotel|stay|map|pin)|\b(?:pics?|photos?|pictures?|shots?)\b)`)
	sendItRe         = regexp.MustCompile(`(?i)^\s*(?:(?:ok|okay|sure|perfect|yes|yeah|please)[,!]?\s+)*(?:send (?:it|them|that|those|the (?:map|pin|link))|(?:send|show)(?:\s+\w+){0,3}\s+(?:map|pin))\b`)
	whoPaysRe        = regexp.MustCompile(`(?i)\bwho(?:'?s| is) paying\b|\bwho(?:'?s| is) (?:putting|on) the card\b`)
	iPayRe           = regexp.MustCompile(`(?i)\b(i('ll| will) (pay|cover|get (this|it))|i('m| am) paying|charge (it to )?me|put it on me|i'll get (the|this))\b`)
	statusAskRe      = regexp.MustCompile(`(?i)\b(update me|updates?\b|what'?s (?:going on|locked|the (?:status|plan|quote)|booked)|status of (?:the )?trip|recap|where are we (?:at|now)|what(?:'s| is) locked|which dates|what dates|when (?:are|do) we (?:go|leave|fly|heading)|how much|talk me through|finances)\b`)
	thanksOnlyRe     = regexp.MustCompile(`(?i)^\s*(?:@\S+\s+)*(?:thanks|thank you|ty|cheers)[\s!.]*$`)
	flightAskRe      = regexp.MustCompile(`(?i)\b(flights?|airfare|airfares|plane tickets?|outbound|return flight|what about the flyin)\b`)
	cheaperAskRe     = regexp.MustCompile(`(?i)\b(cheaper|less expensive|too (?:much|expensive)|lower (?:the )?price|save (?:money|on)|cut (?:the )?cost)\b`)
	foodMoneyRe      = regexp.MustCompile(`(?i)\b(factor in food|include food|food (?:cost|in (?:that|this|the total))|does that include food|is food (?:in|included)|how (?:did we|do you) get to)\b`)
	directQuestionRe = regexp.MustCompile(`(?i)^\s*(?:@\S+\s+)*(what|which|where|when|how|who|why|are there|can we|could you|do we)\b`)
)

type Brain struct {
	Dashboard *dashboard.Manager
	Store     store.Store
	LLM       llm.LLM
	Messenger messaging.Messenger
	Config    *config.Settings

	mentionRe *regexp.Regexp
	locksMu   sync.Mutex
	locks     map[string]*sync.Mutex
	batchMu   sync.Mutex
	batches   map[string]*taggedBatch
}

type taggedBatch struct {
	msgs  []models.IncomingMessage
	timer *time.Timer
}

func NewBrain(cfg *config.Settings, st store.Store, llmClient llm.LLM, messenger messaging.Messenger) *Brain {
	return &Brain{
		Store:     st,
		LLM:       llmClient,
		Messenger: messenger,
		Config:    cfg,
		mentionRe: regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(cfg.BotName) + `\b`),
		locks:     map[string]*sync.Mutex{},
		batches:   map[string]*taggedBatch{},
	}
}

// today is today's date in the team's timezone. Railway runs in UTC, which is already "tomorrow"
// after 5pm in Vancouver, and that would shift "this weekend" by a day.
func (b *Brain) today() time.Time {
	loc, err := time.LoadLocation(b.Config.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return time.Now().In(loc)
}

// validateOptions checks each option against every participant's stated availability. Returns
// {option.destination: [violation, ...]} for options that don't fit someone. Gemini is already
// told these rules (see PROPOSE_SYSTEM); this just catches when it slips.
func validateOptions(options []models.Option, people []models.Participant) map[string][]string {
	violations := map[string][]string{}
	for _, o := range options {
		var issues []string
		for _, p := range people {
			if len(p.GeneralPreferences.Availability) > 0 && !fitsAvailability(o, p.GeneralPreferences.Availability) {
				issues = append(issues, fmt.Sprintf("%s–%s doesn't fit %s's available dates",
					o.EmbarkingDate, o.ReturningDate, p.WhatsAppName))
			}
		}
		if len(issues) > 0 {
			violations[o.Destination] = issues
		}
	}
	return violations
}

// fitsAvailability checks that every day of the option's window is in the participant's
// availability list (a flat list of exact "YYYY-MM-DD" dates, not ranges).
func fitsAvailability(o models.Option, availability []string) bool {
	start, err := models.ParseDate(o.EmbarkingDate)
	if err != nil {
		return false
	}
	end, err := models.ParseDate(o.ReturningDate)
	if err != nil {
		return false
	}
	set := make(map[string]bool, len(availability))
	for _, d := range availability {
		set[d] = true
	}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if !set[d.Format("2006-01-02")] {
			return false
		}
	}
	return true
}

func looksLikeWhatsAppID(s string) bool {
	sl := strings.ToLower(s)
	return strings.Contains(sl, "@g.us") || strings.Contains(sl, "@c.us") ||
		strings.Contains(sl, "@lid") || strings.Contains(sl, "@s.whatsapp")
}

var (
	waJIDRe    = regexp.MustCompile(`@?[A-Za-z0-9._+-]+@(?:c\.us|g\.us|lid|s\.whatsapp\.net)`)
	waAtNumRe  = regexp.MustCompile(`@\d{6,}`)
	waSpacesRe = regexp.MustCompile(`[^\S\n]{2,}`)
)

func scrubWhatsAppIDs(text string) string {
	text = waJIDRe.ReplaceAllString(text, "")
	text = waAtNumRe.ReplaceAllString(text, "")
	return strings.TrimSpace(waSpacesRe.ReplaceAllString(text, " "))
}

func formatGroupRoster(members []models.GroupMember) string {
	var lines []string
	for _, p := range members {
		name := strings.TrimSpace(p.Name)
		if name == "" || p.IsAgent || looksLikeWhatsAppID(name) {
			continue
		}
		lines = append(lines, "- "+name+"  (mention as @"+name+")")
	}
	return strings.Join(lines, "\n")
}

// snapNamesToRoster rewrites Gemini's whatsapp_name values onto the real group
// display names the robot sent, so we don't persist nicknames it invented.
func snapNamesToRoster(people []models.Participant, roster []models.GroupMember) []models.Participant {
	var rosterPeople []models.Participant
	for _, p := range roster {
		name := strings.TrimSpace(p.Name)
		if name == "" || p.IsAgent || looksLikeWhatsAppID(name) {
			continue
		}
		rosterPeople = append(rosterPeople, models.Participant{WhatsAppName: name, PID: p.ID})
	}
	if len(rosterPeople) == 0 {
		return people
	}
	for i := range people {
		if hit := matchName(people[i].WhatsAppName, rosterPeople); hit != nil {
			people[i].WhatsAppName = hit.WhatsAppName
		}
	}
	return people
}

func looksLikeItineraryAsk(text string) bool {
	if restaurantListRe.MatchString(text) {
		return false
	}
	return itineraryAskRe.MatchString(text)
}

func looksLikeRestaurantAsk(text string) bool {
	if restaurantListRe.MatchString(text) {
		return true
	}
	if looksLikeItineraryAsk(text) {
		return false
	}
	return restaurantAskRe.MatchString(text)
}

func looksLikeIntroAsk(text string) bool {
	return introAskRe.MatchString(text)
}

func looksLikeOnlyGreeting(text string) bool {
	stripped := strings.TrimSpace(atTokenRe.ReplaceAllString(text, " "))
	stripped = strings.TrimSpace(stripped)
	return stripped == "" || greetingOnlyRe.MatchString(stripped)
}

func looksLikeSearchAsk(text string) bool {
	return searchAskRe.MatchString(text)
}

func looksLikeNewTripRequest(text string) bool {
	return newTripAskRe.MatchString(text)
}

func looksLikeDirectQuestion(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if strings.Contains(t, "?") {
		return true
	}
	return directQuestionRe.MatchString(t)
}

func looksLikeBookAsk(text string) bool {
	if looksLikeSearchAsk(text) {
		return true
	}
	if approveOnlyRe.MatchString(text) {
		return true
	}
	if confirmPlanRe.MatchString(text) {
		return true
	}
	low := strings.ToLower(text)
	if !bookAskRe.MatchString(low) {
		return false
	}
	return strings.Contains(low, "flight") || strings.Contains(low, "hotel") ||
		strings.Contains(low, "stay") || strings.Contains(low, "trip") ||
		strings.Contains(low, "book it") || strings.Contains(low, "please book") ||
		strings.Contains(low, "go ahead") || strings.Contains(low, "lock it in")
}

func looksLikeHotelAsk(text string) bool {
	if looksLikeBookAsk(text) {
		return false
	}
	return hotelAskRe.MatchString(text)
}

func looksLikeSendIt(text string) bool {
	return sendItRe.MatchString(strings.TrimSpace(text))
}

func looksLikeCheaperAsk(text string) bool {
	return cheaperAskRe.MatchString(text)
}

func looksLikeFoodMoneyAsk(text string) bool {
	return foodMoneyRe.MatchString(text)
}

func looksLikeStatusAsk(text string) bool {
	return statusAskRe.MatchString(text)
}

func looksLikeAdvisorAsk(text string) bool {
	return looksLikeItineraryAsk(text) || looksLikeRestaurantAsk(text) || looksLikeStatusAsk(text) ||
		looksLikeDashboardAsk(text) || looksLikeFoodMoneyAsk(text) || thanksOnlyRe.MatchString(text)
}

func (b *Brain) answerDuringIntake(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	trip = b.syncTripBasics(ctx, trip, m.Text)
	if looksLikeDashboardAsk(m.Text) {
		link := b.tripPageURL(ctx, trip, "")
		if link != "" {
			return b.say(ctx, trip.GroupID, "Here's the trip page:\n"+link, nil)
		}
	}
	if looksLikeItineraryAsk(m.Text) {
		if looksLikeRestaurantAsk(m.Text) {
			if err := b.writeRestaurantPlan(ctx, trip, m); err != nil {
				slog.Warn("restaurant list failed", "group_id", trip.GroupID, "err", err)
			}
		}
		return b.writeAdvisorItinerary(ctx, trip, m)
	}
	if looksLikeRestaurantAsk(m.Text) {
		return b.writeRestaurantPlan(ctx, trip, m)
	}
	return b.answerQuestion(ctx, trip, m)
}

func looksLikeFlightAsk(text string) bool {
	if looksLikeBookAsk(text) || looksLikeSearchAsk(text) {
		return false
	}
	return flightAskRe.MatchString(text)
}

func looksLikeWhoPays(text string) bool {
	return whoPaysRe.MatchString(text)
}

func looksLikeIPay(text string) bool {
	return iPayRe.MatchString(text)
}

func quotedText(m models.IncomingMessage) string {
	if m.Quoted == nil {
		return ""
	}
	return strings.TrimSpace(m.Quoted.Text)
}

// wantsHotelMap is only for the current line asking for the stay — never because
// a quoted bot line happened to mention the hotel or a map.
func wantsHotelMap(trip *models.Trip, m models.IncomingMessage) bool {
	if looksLikeStatusAsk(m.Text) || looksLikeFlightAsk(m.Text) || looksLikeFoodMoneyAsk(m.Text) {
		return false
	}
	if looksLikeSendIt(m.Text) {
		return true
	}
	return looksLikeHotelAsk(m.Text)
}

func looksLikePrefUpdate(text string, people []models.Participant, roster []models.GroupMember) bool {
	if looksLikeItineraryAsk(text) || looksLikeRestaurantAsk(text) || looksLikeIntroAsk(text) || looksLikeOnlyGreeting(text) {
		return false
	}
	if proxyPrefRe.MatchString(text) {
		return true
	}
	if !prefFactRe.MatchString(text) {
		return false
	}
	low := strings.ToLower(text)
	var names []string
	for _, p := range people {
		names = append(names, p.WhatsAppName)
	}
	for _, m := range roster {
		if m.IsAgent {
			continue
		}
		names = append(names, m.Name)
	}
	for _, n := range names {
		fields := strings.Fields(strings.TrimSpace(n))
		if len(fields) == 0 {
			continue
		}
		first := strings.ToLower(fields[0])
		if len(first) < 3 {
			continue
		}
		if strings.Contains(low, first) {
			return true
		}
	}
	return false
}

// designatedPayer returns the WhatsAppName of the participant marked as payer, or "" if
// nobody's been asked yet (the group never answered the "who's paying?" poll).
func designatedPayer(people []models.Participant) string {
	for _, p := range people {
		if p.Payer {
			return p.WhatsAppName
		}
	}
	return ""
}

// matchName matches a chat display name to an extracted participant. Chat names and Gemini's
// extracted names often disagree on nicknames or a last name ("Jordan Lee" in chat vs "Jordan"
// extracted, or the reverse), so try exact, then substring, then first-name before giving up.
func matchName(name string, people []models.Participant) *models.Participant {
	needle := strings.ToLower(strings.TrimSpace(name))
	for i := range people {
		if strings.ToLower(strings.TrimSpace(people[i].WhatsAppName)) == needle {
			return &people[i]
		}
	}
	for i := range people {
		hay := strings.ToLower(strings.TrimSpace(people[i].WhatsAppName))
		if strings.Contains(needle, hay) || strings.Contains(hay, needle) {
			return &people[i]
		}
	}
	needleFields := strings.Fields(needle)
	if len(needleFields) == 0 {
		return nil
	}
	for i := range people {
		fields := strings.Fields(strings.ToLower(people[i].WhatsAppName))
		if len(fields) > 0 && fields[0] == needleFields[0] {
			return &people[i]
		}
	}
	return nil
}

// ------------------------------------------------------------------ entry point

func (b *Brain) lockFor(groupID string) *sync.Mutex {
	b.locksMu.Lock()
	defer b.locksMu.Unlock()
	l, ok := b.locks[groupID]
	if !ok {
		l = &sync.Mutex{}
		b.locks[groupID] = l
	}
	return l
}

// Handle is the entry point for every incoming message: one message at a time per group.
func (b *Brain) Handle(ctx context.Context, m models.IncomingMessage) {
	if !m.Tagged && b.mentionRe.MatchString(m.Text) {
		m.Tagged = true
	}
	if shouldBatchMention(m) {
		b.enqueueTagged(ctx, m)
		return
	}
	b.handleLocked(ctx, m)
}

func shouldBatchMention(m models.IncomingMessage) bool {
	if !m.Tagged || strings.TrimSpace(m.Text) == "" {
		return false
	}
	// Each explicit trip request gets its own session, even in a burst of group messages.
	if looksLikeNewTripRequest(m.Text) {
		return false
	}
	if choiceOnlyRe.MatchString(m.Text) || approveOnlyRe.MatchString(m.Text) || rejectOnlyRe.MatchString(m.Text) {
		return false
	}
	if looksLikeIntroAsk(m.Text) || looksLikeOnlyGreeting(m.Text) || looksLikeDashboardAsk(m.Text) || looksLikeBookAsk(m.Text) {
		return false
	}
	return true
}

func (b *Brain) enqueueTagged(ctx context.Context, m models.IncomingMessage) {
	const wait = 1500 * time.Millisecond
	b.batchMu.Lock()
	defer b.batchMu.Unlock()
	batch := b.batches[m.GroupID]
	if batch == nil {
		batch = &taggedBatch{}
		b.batches[m.GroupID] = batch
	}
	batch.msgs = append(batch.msgs, m)
	if batch.timer != nil {
		batch.timer.Stop()
	}
	groupID := m.GroupID
	batch.timer = time.AfterFunc(wait, func() {
		b.batchMu.Lock()
		cur := b.batches[groupID]
		var msgs []models.IncomingMessage
		if cur != nil {
			msgs = append([]models.IncomingMessage(nil), cur.msgs...)
			delete(b.batches, groupID)
		}
		b.batchMu.Unlock()
		if len(msgs) == 0 {
			return
		}
		b.handleLocked(ctx, mergeTagged(msgs))
	})
}

func mergeTagged(msgs []models.IncomingMessage) models.IncomingMessage {
	out := msgs[len(msgs)-1]
	if len(msgs) == 1 {
		return out
	}
	seen := map[string]bool{}
	var names []string
	var lines []string
	for _, msg := range msgs {
		who := strings.TrimSpace(msg.SenderName)
		if who == "" {
			who = "Someone"
		}
		if !seen[strings.ToLower(who)] {
			seen[strings.ToLower(who)] = true
			names = append(names, who)
		}
		lines = append(lines, who+": "+strings.TrimSpace(msg.Text))
	}
	out.Text = strings.Join(lines, "\n")
	out.Tagged = true
	out.CoAskers = names
	return out
}

func (b *Brain) handleLocked(ctx context.Context, m models.IncomingMessage) {
	lock := b.lockFor(m.GroupID)
	lock.Lock()
	defer lock.Unlock()
	ctx = messaging.WithReplyTo(ctx, m.MessageID)

	if err := b.handle(ctx, m); err != nil {
		if b.Dashboard != nil {
			_ = b.Dashboard.Emit(ctx, m.GroupID, "session.failed", map[string]any{"message": "Trip planning could not finish. Please try again in the group chat."})
		}
		slog.Error("failed handling message", "group_id", m.GroupID, "err", err)
		if sayErr := b.say(ctx, m.GroupID, geminiFailTalk(err), nil); sayErr != nil {
			slog.Error("failed to send oops message", "group_id", m.GroupID, "err", sayErr)
		}
	}
}

func geminiFailTalk(err error) string {
	if err == nil {
		return "Something broke on my end. Try that again."
	}
	s := err.Error()
	if strings.Contains(s, "503") || strings.Contains(s, "429") || strings.Contains(strings.ToLower(s), "high demand") {
		return "Google's model is slammed right now. Ping me again in a minute and I'll pick it up."
	}
	return "Something broke on my end. Try that again."
}

func (b *Brain) handle(ctx context.Context, m models.IncomingMessage) error {
	// Fallback mention detection: WhatsApp uses several ID formats for one account, so the
	// robot's mentionedIds check can miss. Typed "@Fare" in the text always counts.
	if !m.Tagged && b.mentionRe.MatchString(m.Text) {
		m.Tagged = true
	}
	// Trip is a singleton per group: the document's own ID is the group ID.
	trip, err := b.Store.GetTrip(ctx, m.GroupID)
	if err != nil {
		return err
	}
	st := ""
	if trip != nil {
		st = string(trip.State)
	}
	slog.Info("incoming", "from", m.SenderName, "tagged", m.Tagged, "state", st, "text", clipLog(m.Text, 100))
	sentAt := models.Now()
	if m.Timestamp != 0 {
		sentAt = time.Unix(m.Timestamp, 0).UTC()
	}
	tripID := ""
	if trip != nil {
		tripID = trip.ID
	}
	isNew, err := b.Store.SaveMessage(ctx, &models.Message{
		GroupID: m.GroupID, TripID: tripID, ExternalID: m.MessageID, SenderID: m.SenderID,
		SenderName: m.SenderName, Text: m.Text, Tagged: m.Tagged, SentAt: sentAt,
	})
	if err != nil {
		return err
	}
	if !isNew {
		return nil // duplicate delivery (webhook retry)
	}

	_, _ = b.Store.SaveWhatsAppSession(ctx, "group:"+m.GroupID, map[string]any{
		"trip_id":  m.GroupID,
		"group":    m.GroupName,
		"agent_id": m.AgentID,
	})

	if trip == nil && m.Tagged && looksLikeNewTripRequest(m.Text) {
		return b.startNewTrip(ctx, nil, m, sentAt)
	}
	if trip != nil {
		var handled bool
		trip, m, handled, err = b.routeTripMessage(ctx, trip, m, sentAt)
		if err != nil || handled {
			return err
		}
	}

	if trip != nil {
		if b.Dashboard != nil && shouldBatchMention(m) && (trip.State == models.Collecting || trip.State == models.AwaitingChoice) {
			if _, err := b.Dashboard.Begin(ctx, trip); err != nil {
				return err
			}
		}
		b.rememberRoster(ctx, trip, m)
		trip, err = b.capturePayer(ctx, trip, m)
		if err != nil {
			return err
		}
	}

	if trip == nil {
		trip, err = b.Store.CreateTrip(ctx, m.GroupID, m.GroupName)
		if err != nil {
			return err
		}
		// Create the dashboard session before introductory replies or planning
		// can fail. The frontend should see the request before searches begin.
		if b.Dashboard != nil && shouldBatchMention(m) {
			if _, err := b.Dashboard.Begin(ctx, trip); err != nil {
				return err
			}
		}
		b.rememberRoster(ctx, trip, m)
		trip, err = b.capturePayer(ctx, trip, m)
		if err != nil {
			return err
		}
		stop, trip, err := b.maybeIntroduce(ctx, trip, m)
		if err != nil || stop {
			return err
		}
		if !m.Tagged && !looksLikePrefUpdate(m.Text, trip.Participants, m.Participants) {
			return nil
		}
		// No book-ask shortcut here: a brand-new trip has no chosen option yet, so "let's book" /
		// "sounds good" said during intake can only be answering the current intake question —
		// runIntakeTurn's own extraction.Approval handles an explicit ✅ at the readiness gate.
		return b.runIntakeTurn(ctx, trip, m)
	}

	if trip.State == models.Booked {
		if looksLikeAdvisorAsk(m.Text) && !looksLikeReplan(m.Text) && !looksLikeCancelBooking(m.Text) && (m.Tagged || looksLikeDirectQuestion(m.Text)) {
			return b.answerDuringIntake(ctx, trip, m)
		}
		if looksLikeDirectQuestion(m.Text) && !looksLikeReplan(m.Text) && !looksLikeCancelBooking(m.Text) {
			return b.answerQuestion(ctx, trip, m)
		}
		if !m.Tagged {
			return nil
		}
		stop, trip, err := b.maybeIntroduce(ctx, trip, m)
		if err != nil || stop {
			return err
		}
		if looksLikeCancelBooking(m.Text) || looksLikeReplan(m.Text) {
			recent, _ := b.Store.GetMessages(ctx, m.GroupID, nil, 20, false)
			alreadyCancel := looksLikeCancelBooking(m.Text)
			for _, msg := range recent {
				if looksLikeCancelBooking(msg.Text) {
					alreadyCancel = true
					break
				}
			}
			h := harvestText(m.Text, b.today())
			if alreadyCancel || h.Destination == "" {
				return b.reopenAndPlan(ctx, trip, m)
			}
			old := trip.Destination
			if old == "" {
				old = "the current trip"
			}
			return b.startChangePoll(ctx, trip, m, &models.PendingChange{
				Kind:        "reopen",
				Summary:     fmt.Sprintf("Cancel %s and plan %s instead?", old, h.Destination),
				Destination: h.Destination,
				Dates:       h.Dates,
			})
		}
		return b.answerQuestion(ctx, trip, m)
	}
	if trip.State == models.Cancelled {
		if m.Tagged || looksLikePrefUpdate(m.Text, trip.Participants, m.Participants) {
			trip, err = store.SetState(ctx, b.Store, trip.ID, models.Collecting, map[string]any{"introduced": false})
			if err != nil {
				return err
			}
			if m.Tagged {
				var stop bool
				stop, trip, err = b.maybeIntroduce(ctx, trip, m)
				if err != nil || stop {
					return err
				}
			}
			return b.runIntakeTurn(ctx, trip, m)
		}
		return nil
	}

	slog.Info("trip loaded", "group", m.GroupID, "state", trip.State,
		"people", len(trip.Participants), "options", len(trip.Options),
		"origin", trip.Origin, "dates", hasAnyDates(trip.Participants))

	if m.Tagged {
		var stop bool
		stop, trip, err = b.maybeIntroduce(ctx, trip, m)
		if err != nil || stop {
			return err
		}
	}

	// Scoped to non-Collecting states: during intake there's no chosen option yet, so "let's
	// book"/"sounds good" phrasing can only be answering the current intake question, not a real
	// book-ask. runIntakeTurn's own extraction.Approval handles an explicit ✅ at the readiness gate.
	if trip.State != models.Collecting && (looksLikeSearchAsk(m.Text) || looksLikeBookAsk(m.Text)) {
		return b.handleBookAsk(ctx, trip, m)
	}

	switch trip.State {
	case models.Collecting:
		if trip.PendingQuestion != nil || strongAttendanceYes(m.Text) || m.Tagged || looksLikePrefUpdate(m.Text, trip.Participants, m.Participants) {
			return b.runIntakeTurn(ctx, trip, m)
		}
		if looksLikeDirectQuestion(m.Text) {
			return b.answerQuestion(ctx, trip, m)
		}
		return nil
	case models.AwaitingChoice, models.AwaitingApproval:
		return b.onReply(ctx, trip, m)
	case models.Searching, models.BookingState:
		if looksLikeAdvisorAsk(m.Text) && (m.Tagged || looksLikeDirectQuestion(m.Text)) {
			return b.answerDuringIntake(ctx, trip, m)
		}
		if m.Tagged || looksLikeDirectQuestion(m.Text) {
			return b.answerQuestion(ctx, trip, m)
		}
	}
	return nil
}

// ------------------------------------------------------------------ stage: plan

// startNewTrip makes the latest request the group's active trip without requiring
// approval or cancellation of the previous plan. Earlier dashboard snapshots remain intact.
func (b *Brain) startNewTrip(ctx context.Context, previous *models.Trip, m models.IncomingMessage, sentAt time.Time) error {
	groupName := m.GroupName
	var roster []models.GroupMember
	if previous != nil {
		if groupName == "" {
			groupName = previous.GroupName
		}
		roster = previous.Roster
	}
	var trip *models.Trip
	var err error
	if previous == nil {
		trip, err = b.Store.CreateTrip(ctx, m.GroupID, groupName)
	} else {
		trip, err = b.Store.ResetTrip(ctx, previous.ID, groupName)
	}
	if err != nil {
		return err
	}
	if trip == nil {
		return fmt.Errorf("could not start a trip for group %s", m.GroupID)
	}
	// Include the triggering message and exclude the previous trip's conversation.
	trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{
		"history_start": sentAt.Add(-time.Nanosecond),
		"roster":        roster,
	})
	if err != nil {
		return err
	}
	if b.Dashboard != nil {
		if _, err := b.Dashboard.BeginNew(ctx, trip); err != nil {
			return err
		}
	}
	b.rememberRoster(ctx, trip, m)
	trip, err = b.capturePayer(ctx, trip, m)
	if err != nil {
		return err
	}
	return b.runIntakeTurn(ctx, trip, m)
}

func (b *Brain) plan(ctx context.Context, trip *models.Trip, feedback string, incoming models.IncomingMessage) error {
	if shouldUseIntake(trip) {
		return b.runIntakeTurn(ctx, trip, incoming)
	}
	if b.Dashboard != nil {
		if _, err := b.Dashboard.Begin(ctx, trip); err != nil {
			return err
		}
	}
	history, err := b.Store.GetMessages(ctx, trip.GroupID, trip.HistoryStart, 1000, true)
	if err != nil {
		return err
	}
	day := b.today().Format("2006-01-02")
	harvested := harvestFacts(history, b.today())
	latest := strings.TrimSpace(incoming.Text)
	if feedback != "" {
		latest = strings.TrimSpace(feedback)
	}

	userContent := fmt.Sprintf("Latest WhatsApp message from %s:\n%s\n\nSatisfy that message. Older chat is background.\n\n", incoming.SenderName, latest)
	userContent += fmt.Sprintf("Group: %s\nTrip state: %s\n", trip.GroupName, trip.State)
	if trip.Origin != "" || harvested.Airport != "" || harvested.City != "" {
		userContent += fmt.Sprintf("Known origin: %s %s %s\n", trip.Origin, harvested.Airport, harvested.City)
	}
	if hasAnyDates(trip.Participants) || len(harvested.Dates) > 0 {
		userContent += "Known dates (already given — do not ask again): " + strings.Join(unionDates(allStoredDates(trip.Participants), harvested.Dates), ", ") + "\n"
	}
	userContent += "If the latest update says a previous booking is cancelled, ignore that old destination completely. Do not mention it except to acknowledge it's off.\n\n"
	if roster := formatGroupRoster(incoming.Participants); roster != "" {
		userContent += "People actually in this WhatsApp group (use these exact names for whatsapp_name; skip the bot):\n" + roster + "\n\n"
	}
	if known := formatKnownPrefs(trip.Participants); known != "" {
		userContent += "Already stored from earlier in this trip (keep these; only update what the new chat actually changes; do not re-ask for fields that are filled):\n" + known + "\n\n"
	}
	if len(trip.Options) > 0 {
		b, _ := json.MarshalIndent(trip.Options, "", "  ")
		userContent += "Options already shown to the group (do not pretend you haven't proposed these unless they asked to change them):\n" + string(b) + "\n\n"
	}
	userContent += "Recent chat:\n" + compactChat(history, 20)
	if feedback != "" {
		userContent += "\n\nLatest update from the group:\n" + feedback
	}

	slog.Info("calling gemini", "stage", "plan", "from", incoming.SenderName, "text", clipLog(incoming.Text, 80))
	planned, err := b.LLM.Structured(ctx, prompts.PlanSystem(b.Config.BotName, day),
		[]llm.Message{{Role: "user", Content: userContent}},
		toSchema(prompts.PlanTrip))
	if err != nil {
		return err
	}

	var people []models.Participant
	if err := decodeInto(planned["participants"], &people); err != nil {
		return err
	}
	people = snapNamesToRoster(people, incoming.Participants)
	people = mergeParticipants(trip.Participants, people)
	people = applyHarvest(people, harvested, incoming.Participants)
	if len(people) == 0 {
		people = applyHarvest(nil, harvested, incoming.Participants)
	}
	if len(people) == 0 {
		people = append([]models.Participant(nil), trip.Participants...)
	}
	if len(people) == 0 {
		return b.runIntakeTurn(ctx, trip, incoming)
	}
	for i := range people {
		if people[i].PID == "" {
			people[i].PID = fmt.Sprintf("p_%d_%d", time.Now().UnixNano(), i)
		}
	}
	fields := map[string]any{"participants": people}
	if origin := people[0].Origin; origin != "" {
		fields["origin"] = origin
	} else if people[0].OriginCity != "" {
		fields["origin"] = people[0].OriginCity
	} else if people[0].OriginAirport != "" {
		fields["origin"] = people[0].OriginAirport
	}
	if harvested.Destination != "" && trip.Destination == "" {
		fields["destination"] = harvested.Destination
	}
	if harvested.Budget != "" && trip.BudgetNote == "" {
		fields["budget_note"] = harvested.Budget
	}
	trip, err = b.Store.UpdateTrip(ctx, trip.ID, fields)
	if err != nil {
		return err
	}

	if !hasSharedOrigin(people) {
		if trip.AskedOrigin {
			slog.Info("skipping origin re-ask", "trip", trip.ID)
		} else {
			if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"asked_origin": true}); err != nil {
				return err
			}
			return b.askOrigin(ctx, trip)
		}
	} else if trip.AskedOrigin {
		_, _ = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"asked_origin": false})
	}
	if !hasAnyDates(people) {
		if trip.AskedDates {
			slog.Info("skipping dates re-ask", "trip", trip.ID)
		} else {
			if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"asked_dates": true}); err != nil {
				return err
			}
			return b.askDates(ctx, trip)
		}
	} else if trip.AskedDates {
		_, _ = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"asked_dates": false})
	}
	if !enoughToPlan(people) {
		return nil
	}
	if tripDestination(trip) != "" && len(trip.Options) > 0 && !looksLikeReplan(incoming.Text) {
		return b.answerQuestion(ctx, trip, incoming)
	}

	var options []models.Option
	if rawOptions, ok := planned["options"].([]any); ok {
		for _, raw := range rawOptions {
			if len(options) >= 3 {
				break
			}
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			opt, err := optionFromMap(len(options)+1, m)
			if err != nil {
				slog.Warn("dropping invalid option", "option", m, "err", err)
				continue
			}
			options = append(options, opt)
		}
	}

	intro, _ := planned["intro"].(string)
	if len(options) == 0 {
		var ok bool
		trip, intro, options, ok, err = b.propose(ctx, trip, people, day, feedback, true)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	} else {
		trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"options": options})
		if err != nil {
			return err
		}
	}

	if trip.State != models.AwaitingChoice {
		trip, err = store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil)
		if err != nil {
			return err
		}
	}
	return b.postOptions(ctx, trip, intro, options)
}

// propose asks Gemini for 2-3 options, persists them onto the trip, and returns the updated trip.
// If every option breaks someone's dates, it re-asks once with the specifics instead of showing
// the group a plan nobody can actually take. ok=false means a message was already sent and
// there's nothing more to do.
func (b *Brain) propose(ctx context.Context, trip *models.Trip, people []models.Participant, day, feedback string, retried bool) (*models.Trip, string, []models.Option, bool, error) {
	fb := ""
	if feedback != "" {
		fb = "\nThe group asked for changes: " + feedback
	}
	lines := make([]string, len(people))
	for i, p := range people {
		pj, err := json.Marshal(p)
		if err != nil {
			return trip, "", nil, false, err
		}
		lines[i] = string(pj)
	}
	content := "Participants:\n" + strings.Join(lines, "\n")

	slog.Info("calling gemini", "stage", "propose", "trip", trip.ID)
	proposal, err := b.LLM.Structured(ctx, prompts.ProposeSystem(b.Config.BotName, day, fb),
		[]llm.Message{{Role: "user", Content: content}}, toSchema(prompts.ProposeOptions))
	if err != nil {
		return trip, "", nil, false, err
	}

	rawOptions, _ := proposal["options"].([]any)
	var options []models.Option
	for _, raw := range rawOptions {
		if len(options) >= 3 {
			break
		}
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// structured outputs can't enforce date formats, so a bad option is skipped, not fatal
		opt, err := optionFromMap(len(options)+1, m)
		if err != nil {
			slog.Warn("dropping invalid option", "option", m, "err", err)
			continue
		}
		options = append(options, opt)
	}
	if len(options) == 0 {
		return trip, "", nil, false, b.askDates(ctx, trip)
	}

	violations := validateOptions(options, people)
	if len(violations) == len(options) && !retried {
		slog.Info("trip: options may not fit every date window", "trip", trip.ID)
	}

	trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"options": options})
	if err != nil {
		return trip, "", nil, false, err
	}
	intro, _ := proposal["intro"].(string)
	return trip, intro, options, true, nil
}

func optionFromMap(position int, m map[string]any) (models.Option, error) {
	var o models.Option
	o.Position = position
	var err error
	if o.Destination, err = requiredString(m, "destination"); err != nil {
		return o, err
	}
	if o.DestinationAirport, err = requiredString(m, "destination_airport"); err != nil {
		return o, err
	}
	if o.ActivityDescription, err = requiredString(m, "activity_description"); err != nil {
		return o, err
	}
	if o.CulinaryDescription, err = requiredString(m, "culinary_description"); err != nil {
		return o, err
	}
	if o.EmbarkingDate, err = requiredString(m, "embarking_date"); err != nil {
		return o, err
	}
	if o.ReturningDate, err = requiredString(m, "returning_date"); err != nil {
		return o, err
	}
	if o.WhyItWorks, err = requiredString(m, "why_it_works"); err != nil {
		return o, err
	}
	if o.Tradeoffs, err = requiredString(m, "tradeoffs"); err != nil {
		return o, err
	}
	if v, ok := m["duration_nights"].(float64); ok {
		o.DurationNights = int(v)
	}
	if v, ok := m["cost_per_person"].(float64); ok {
		o.CostPerPerson = &v
	}
	if _, err := models.ParseDate(o.EmbarkingDate); err != nil {
		return o, fmt.Errorf("bad embarking_date: %w", err)
	}
	if _, err := models.ParseDate(o.ReturningDate); err != nil {
		return o, fmt.Errorf("bad returning_date: %w", err)
	}
	return o, nil
}

func requiredString(m map[string]any, key string) (string, error) {
	v, ok := m[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("missing %s", key)
	}
	return v, nil
}

// replan re-proposes options for already-known participants ("make it cheaper", "swap to the
// beach one"). No re-extraction: the group is reacting to the options, not restating
// preferences, so re-reading the whole chat through Gemini again would just add latency.
func (b *Brain) replan(ctx context.Context, trip *models.Trip, feedback string) error {
	trip, intro, options, ok, err := b.propose(ctx, trip, trip.Participants, b.today().Format("2006-01-02"), feedback, false)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return b.postOptions(ctx, trip, intro, options)
}

// ------------------------------------------------------------------ replies

func (b *Brain) onReply(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	intent, err := b.interpret(ctx, trip, m)
	if err != nil {
		return err
	}
	if intent == nil {
		return nil
	}
	kind, _ := intent["intent"].(string)
	slog.Info("trip intent", "trip", trip.ID, "state", trip.State, "intent", kind)

	switch {
	case kind == "choose" && trip.State == models.AwaitingChoice:
		num := intentNumber(intent["option_number"])
		return b.selectOption(ctx, trip, num)
	case kind == "approve" && trip.State == models.AwaitingApproval:
		return b.requestBooking(ctx, trip, m.SenderName)
	case kind == "reject" && trip.State == models.AwaitingApproval:
		trip, err = store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil)
		if err != nil {
			return err
		}
		return b.postOptions(ctx, trip, "No worries — here they are again.", trip.Options)
	case kind == "revise":
		if trip.State == models.AwaitingApproval {
			trip, err = store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil)
			if err != nil {
				return err
			}
		}
		revision, _ := intent["revision_request"].(string)
		if revision == "" {
			revision = m.Text
		}
		return b.plan(ctx, trip, revision, m)
	case kind == "cancel":
		if _, err := store.SetState(ctx, b.Store, trip.ID, models.Cancelled, nil); err != nil {
			return err
		}
		return b.say(ctx, trip.GroupID, "Okay, dropping this trip. Ping me if you want to start over.", nil)
	case kind == "cheaper":
		return b.offerCheaperFlights(ctx, trip, m)
	case kind == "question", kind == "other":
		if looksLikeAdvisorAsk(m.Text) {
			return b.answerDuringIntake(ctx, trip, m)
		}
		return b.answerQuestion(ctx, trip, m)
	case kind == "approve" && trip.State == models.AwaitingChoice:
		return b.handleBookAsk(ctx, trip, m)
	case kind == "search":
		return b.handleBookAsk(ctx, trip, m)
	}
	return nil
}

func intentNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

// interpret uses regex only for 1/2/3 and yes/no. Tagged chat goes to Gemini.
func (b *Brain) interpret(ctx context.Context, trip *models.Trip, m models.IncomingMessage) (map[string]any, error) {
	if trip.State == models.AwaitingChoice {
		if hit := choiceOnlyRe.FindStringSubmatch(m.Text); hit != nil {
			n, _ := strconv.Atoi(hit[1])
			return map[string]any{"intent": "choose", "option_number": float64(n)}, nil
		}
	}
	if trip.State == models.AwaitingApproval && (approveOnlyRe.MatchString(m.Text) || looksLikeBookAsk(m.Text)) {
		return map[string]any{"intent": "approve"}, nil
	}
	if trip.State == models.AwaitingApproval {
		if rejectOnlyRe.MatchString(m.Text) {
			return map[string]any{"intent": "reject"}, nil
		}
	}
	if looksLikeSearchAsk(m.Text) {
		return map[string]any{"intent": "search"}, nil
	}
	if looksLikeBookAsk(m.Text) {
		if trip.State == models.AwaitingApproval {
			return map[string]any{"intent": "approve"}, nil
		}
		return map[string]any{"intent": "search"}, nil
	}
	if !m.Tagged && !looksLikeDirectQuestion(m.Text) {
		return nil, nil
	}

	optLabels := "none"
	if len(trip.Options) > 0 {
		parts := make([]string, 0, len(trip.Options))
		for _, o := range trip.Options {
			parts = append(parts, fmt.Sprintf("%d:%s", o.Position, o.Destination))
		}
		optLabels = strings.Join(parts, ", ")
	}
	user := fmt.Sprintf("Latest WhatsApp message from %s:\n%s\nTrip destination: %s\nDates: %s to %s\n",
		m.SenderName, m.Text, tripDestination(trip), trip.EmbarkingDate, trip.ReturningDate)
	out, err := b.LLM.Structured(ctx, prompts.InterpretSystem(string(trip.State), optLabels),
		[]llm.Message{{Role: "user", Content: user}}, toSchema(prompts.InterpretReply))
	if err != nil {
		return map[string]any{"intent": "question"}, nil
	}
	if _, ok := out["intent"].(string); !ok {
		out["intent"] = "question"
	}
	return out, nil
}

func tripHasLockedFares(trip *models.Trip) bool {
	if trip == nil {
		return false
	}
	if trip.FlightsLocked {
		return true
	}
	if trip.Itinerary != nil {
		if _, ok := trip.Itinerary["flights"]; ok {
			return true
		}
	}
	return trip.State == models.AwaitingApproval || trip.State == models.Booked
}

func tripFacts(trip *models.Trip) map[string]any {
	facts := map[string]any{
		"state":               trip.State,
		"origin":              trip.Origin,
		"destination":         trip.Destination,
		"destination_airport": trip.DestinationAirport,
		"dates":               []string{trip.EmbarkingDate, trip.ReturningDate},
		"cost_per_person_cad": trip.CostPerPerson,
		"duration_nights":     trip.DurationNights,
		"budget_note_cad":     trip.BudgetNote,
		"flights_locked":      trip.FlightsLocked,
		"headcount":           len(trip.Participants),
	}
	if spend := formatting.ComputeSpend(trip); spend.Ok() {
		facts["locked_spend"] = map[string]any{
			"flight_round_trip_each_cad":   spend.FlightEach,
			"hotel_group_cad":              spend.HotelGroup,
			"hotel_each_cad":               spend.HotelEach,
			"flights_plus_hotel_each_cad":  spend.TravelEach,
			"flights_plus_hotel_group_cad": spend.TravelGroup,
			"people":                       spend.People,
			"includes_food":                false,
		}
		facts["cost_covers"] = "flights_and_hotel_only"
		facts["price_rule"] = "Use locked_spend only for flight/hotel money. Do not quote trip.cost_per_person or option guesses. Food is extra."
	}
	if trip.Itinerary != nil {
		facts["locked"] = map[string]any{
			"flights":     trip.Itinerary["flights"],
			"hotel":       trip.Itinerary["hotel"],
			"restaurants": trip.Itinerary["restaurants"],
		}
		if _, ok := facts["price_rule"]; !ok {
			facts["price_rule"] = "Only quote numbers from locked_spend. Option list prices are guesses."
		}
	} else {
		guesses := make([]map[string]any, 0, len(trip.Options))
		for _, o := range trip.Options {
			guesses = append(guesses, map[string]any{
				"position": o.Position, "destination": o.Destination,
				"rough_guess_cad": o.CostPerPerson, "dates": []string{o.EmbarkingDate, o.ReturningDate},
			})
		}
		facts["option_guesses"] = guesses
		facts["price_rule"] = "Those option figures are rough guesses, not searched fares. Do not present them as quotes."
	}
	return facts
}

func (b *Brain) answerQuestion(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	if looksLikeDashboardAsk(m.Text) {
		link := b.tripPageURL(ctx, trip, "")
		if link != "" {
			return b.say(ctx, trip.GroupID, "Here's the trip page:\n"+link, nil)
		}
	}
	facts, err := json.Marshal(tripFacts(trip))
	if err != nil {
		return err
	}
	history, _ := b.Store.GetMessages(ctx, trip.GroupID, trip.HistoryStart, 40, true)
	user := fmt.Sprintf("Latest WhatsApp message from %s:\n%s\n", m.SenderName, m.Text)
	if q := quotedText(m); q != "" {
		user += "They are quoting this bot/group line:\n" + q + "\n"
	}
	user += "\nLOCKED TRIP FACTS (JSON). Use these. Inventing prices or airports is not allowed.\n" + string(facts)
	user += "\n\nRecent chat:\n" + compactChat(history, 24)
	user += "\n\nReply to the latest message only. Never claim you are sending a photo or picture. If they asked about the hotel, name the stay — a Google Maps pin is sent separately."
	slog.Info("calling gemini", "stage", "reply", "from", m.SenderName, "text", clipLog(m.Text, 80))
	answer, err := b.LLM.Agent(ctx, prompts.AgentSystem(b.Config.BotName, string(facts)),
		[]llm.Message{{Role: "user", Content: user}},
		nil, nil)
	if err != nil {
		return err
	}
	return b.say(ctx, trip.GroupID, answer, nil)
}

func (b *Brain) postLockedStatus(ctx context.Context, trip *models.Trip) error {
	if spend := formatting.ComputeSpend(trip); spend.Ok() {
		fields := map[string]any{"cost_per_person": spend.TravelEach}
		itin := trip.Itinerary
		if itin == nil {
			itin = map[string]any{}
		}
		itin["locked_spend"] = map[string]any{
			"flight_round_trip_each_cad":   spend.FlightEach,
			"hotel_group_cad":              spend.HotelGroup,
			"hotel_each_cad":               spend.HotelEach,
			"flights_plus_hotel_each_cad":  spend.TravelEach,
			"flights_plus_hotel_group_cad": spend.TravelGroup,
			"people":                       spend.People,
			"includes_food":                false,
		}
		fields["itinerary"] = itin
		if updated, err := b.Store.UpdateTrip(ctx, trip.ID, fields); err == nil && updated != nil {
			trip = updated
		}
	}
	text := formatting.LockedStatus(trip)
	if strings.TrimSpace(text) == "" {
		text = "Nothing is locked yet — I still need a destination and a fare search."
	}
	return b.say(ctx, trip.GroupID, text, nil)
}

func (b *Brain) maybeIntroduce(ctx context.Context, trip *models.Trip, m models.IncomingMessage) (bool, *models.Trip, error) {
	if trip == nil || !m.Tagged {
		return false, trip, nil
	}
	force := looksLikeIntroAsk(m.Text)
	if !force && trip.Introduced {
		return false, trip, nil
	}
	if err := b.say(ctx, trip.GroupID, formatting.IntroMessage(b.Config.BotName), nil); err != nil {
		return true, trip, err
	}
	if !trip.Introduced {
		updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"introduced": true})
		if err != nil {
			return true, trip, err
		}
		if updated != nil {
			trip = updated
		} else {
			trip.Introduced = true
		}
	}
	onlyIntro := looksLikeIntroAsk(m.Text) && !looksLikeRestaurantAsk(m.Text) && !looksLikeItineraryAsk(m.Text) &&
		!looksLikeStatusAsk(m.Text) && !looksLikeDashboardAsk(m.Text) && !looksLikePrefUpdate(m.Text, trip.Participants, m.Participants)
	if onlyIntro || looksLikeOnlyGreeting(m.Text) {
		return true, trip, nil
	}
	return false, trip, nil
}

func (b *Brain) replyToAsks(ctx context.Context, trip *models.Trip, m models.IncomingMessage) (bool, error) {
	did := false
	if looksLikeBookAsk(m.Text) {
		if err := b.handleBookAsk(ctx, trip, m); err != nil {
			return true, err
		}
		return true, nil
	}
	if looksLikeDashboardAsk(m.Text) {
		if trip.State == models.Searching || trip.State == models.BookingState || trip.State == models.AwaitingApproval || trip.State == models.Booked {
			if err := b.shareDashboard(ctx, trip); err != nil {
				return true, err
			}
			did = true
		}
	}
	if looksLikeItineraryAsk(m.Text) {
		if err := b.writeAdvisorItinerary(ctx, trip, m); err != nil {
			return true, err
		}
		did = true
		return did, nil
	}
	if looksLikeRestaurantAsk(m.Text) {
		if err := b.writeRestaurantPlan(ctx, trip, m); err != nil {
			return true, err
		}
		did = true
	}
	if wantsHotelMap(trip, m) && !looksLikeCancelBooking(m.Text) {
		if err := b.sendHotelMap(ctx, trip); err != nil {
			return true, err
		}
		did = true
	}
	if looksLikeWhoPays(m.Text) {
		if err := b.maybeAskPayer(ctx, trip, true); err != nil {
			return true, err
		}
		did = true
	}
	if looksLikeCheaperAsk(m.Text) && tripHasLockedFares(trip) && !looksLikeItineraryAsk(m.Text) {
		if err := b.offerCheaperFlights(ctx, trip, m); err != nil {
			return true, err
		}
		did = true
	}
	return did, nil
}

func (b *Brain) handleBookAsk(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	if trip.PendingChange != nil {
		updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_change": (*models.PendingChange)(nil), "last_poll": ""})
		if err != nil {
			return err
		}
		if updated != nil {
			trip = updated
		}
	}
	switch trip.State {
	case models.AwaitingApproval:
		return b.requestBooking(ctx, trip, m.SenderName)
	case models.Searching, models.BookingState:
		_ = b.shareDashboard(ctx, trip)
		return b.say(ctx, trip.GroupID, "Already searching flights and stays. Use the live page I sent to watch it.", nil)
	case models.Booked:
		return b.say(ctx, trip.GroupID, "Already booked (sandbox). Say cancel if you want to start over.", nil)
	default:
		n, err := b.ensureBookableOption(ctx, trip)
		if err != nil {
			return err
		}
		if n == 0 {
			history, histErr := b.Store.GetMessages(ctx, trip.GroupID, trip.HistoryStart, 80, true)
			if histErr == nil {
				h := harvestFacts(history, b.today())
				if h.Destination != "" {
					if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"destination": h.Destination}); err != nil {
						return err
					}
					trip.Destination = h.Destination
					n, err = b.ensureBookableOption(ctx, trip)
					if err != nil {
						return err
					}
				}
			}
		}
		if n == 0 {
			return b.plan(ctx, trip, m.Text, m)
		}
		trip, err = b.lockChosenOption(ctx, trip, n)
		if err != nil {
			return err
		}
		trip = b.syncTripBasics(ctx, trip, m.Text)
		if looksLikeSearchAsk(m.Text) || looksLikeDashboardAsk(m.Text) || strings.Contains(m.Text, "✅") || trip.LastPoll == "finalize" || airportFromLabel(m.Text) != "" {
			return b.startTravelSearch(ctx, trip)
		}
		return b.offerPlanFinalize(ctx, trip, m.Text)
	}
}

func (b *Brain) ensureBookableOption(ctx context.Context, trip *models.Trip) (int, error) {
	if n := bookableOption(trip); n > 0 {
		return n, nil
	}
	dest := tripDestination(trip)
	if dest == "" {
		return 0, nil
	}
	opt := models.Option{
		Position: 1, Destination: dest, DestinationAirport: trip.DestinationAirport,
		DurationNights: trip.DurationNights, EmbarkingDate: trip.EmbarkingDate, ReturningDate: trip.ReturningDate,
		ActivityDescription: trip.ActivityDescription, CulinaryDescription: trip.CulinaryDescription,
	}
	if o := trip.ChosenOption(); o != nil {
		if opt.EmbarkingDate == "" {
			opt.EmbarkingDate = o.EmbarkingDate
		}
		if opt.ReturningDate == "" {
			opt.ReturningDate = o.ReturningDate
		}
		if opt.DestinationAirport == "" {
			opt.DestinationAirport = o.DestinationAirport
		}
		if opt.DurationNights == 0 {
			opt.DurationNights = o.DurationNights
		}
	}
	if opt.DurationNights == 0 {
		opt.DurationNights = tripNights(trip, "")
	}
	opts := append([]models.Option(nil), trip.Options...)
	if len(opts) == 0 {
		opts = []models.Option{opt}
	} else {
		opt.Position = len(opts) + 1
		opts = append(opts, opt)
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"options": opts, "destination": dest})
	if err != nil {
		return 0, err
	}
	if updated != nil {
		*trip = *updated
	}
	return opt.Position, nil
}

func bookableOption(trip *models.Trip) int {
	if trip == nil {
		return 0
	}
	if len(trip.Options) == 1 {
		return trip.Options[0].Position
	}
	dest := strings.TrimSpace(trip.Destination)
	if dest == "" {
		if o := trip.ChosenOption(); o != nil {
			return o.Position
		}
		return 0
	}
	for _, o := range trip.Options {
		if strings.EqualFold(strings.TrimSpace(o.Destination), dest) {
			return o.Position
		}
	}
	if o := trip.ChosenOption(); o != nil && o.Position > 0 {
		return o.Position
	}
	return 0
}

func tripDestination(trip *models.Trip) string {
	if trip == nil {
		return ""
	}
	if trip.Destination != "" {
		return trip.Destination
	}
	if o := trip.ChosenOption(); o != nil && o.Destination != "" {
		return o.Destination
	}
	if len(trip.Options) > 0 && trip.Options[0].Destination != "" {
		return trip.Options[0].Destination
	}
	if trip.Intake != nil && trip.Intake.Destination.Known() {
		return trip.Intake.Destination.AsString()
	}
	return ""
}

func tripNights(trip *models.Trip, asked string) int {
	if n := durationFromText(asked); n > 0 {
		return n
	}
	if trip.DurationNights > 0 {
		return trip.DurationNights
	}
	if o := trip.ChosenOption(); o != nil && o.DurationNights > 0 {
		return o.DurationNights
	}
	if len(trip.Options) > 0 && trip.Options[0].DurationNights > 0 {
		return trip.Options[0].DurationNights
	}
	for _, p := range trip.Participants {
		if p.GeneralPreferences.DurationNights > 0 {
			return p.GeneralPreferences.DurationNights
		}
	}
	return 7
}

func foodBudgetHint(trip *models.Trip, days int) string {
	var lines []string
	lines = append(lines, "Budget on this trip is one general CAD figure, not flight/hotel/food categories. The locked quote is flights + hotel only.")
	if strings.TrimSpace(trip.BudgetNote) != "" {
		lines = append(lines, "Stated group budget note: "+strings.TrimSpace(trip.BudgetNote)+" CAD each.")
	}
	if spend := formatting.ComputeSpend(trip); spend.Ok() {
		lines = append(lines, "Locked flights+hotel: "+spend.TotalLine())
		lines = append(lines, spend.FoodLine())
	}
	if days < 1 {
		days = 7
	}
	lines = append(lines, fmt.Sprintf("Estimate meals for %d days in CAD per person. Food is itinerary-only, rough, not booked.", days))
	return strings.Join(lines, "\n")
}

func advisorMap(trip *models.Trip) map[string]any {
	if trip == nil {
		return nil
	}
	itin := trip.Itinerary
	if itin == nil {
		return nil
	}
	raw, _ := itin["advisor"].(map[string]any)
	return raw
}

func (b *Brain) generateAdvisorItinerary(ctx context.Context, trip *models.Trip, request, from string) (*models.Trip, map[string]any, error) {
	dest := tripDestination(trip)
	if dest == "" {
		return trip, nil, nil
	}
	if out := advisorMap(trip); out != nil && !looksLikeItineraryAsk(request) && !looksLikeRestaurantAsk(request) {
		if days, _ := out["days"].([]any); len(days) > 0 {
			return trip, out, nil
		}
	}
	nights := tripNights(trip, request)
	days := nights
	if days < 1 {
		days = 7
	}
	if days > 10 {
		days = 10
	}
	dates := ""
	if trip.EmbarkingDate != "" {
		dates = formatting.Dates(trip.EmbarkingDate, trip.ReturningDate)
	} else if trip.Intake != nil && trip.Intake.ExactDates != nil {
		dates = formatting.Dates(trip.Intake.ExactDates.Depart, trip.Intake.ExactDates.Return)
	}
	factsJSON, _ := json.Marshal(tripFacts(trip))
	user := fmt.Sprintf("City: %s\nDays: %d\nDates: %s\nOrigin: %s\nTastes: %s\nRequest: %s\n\n%s\nLOCKED FACTS:\n%s\n\nDay 1 arrival must match the inbound flight airport in locked facts. Do not invent fares, hotels, or airports. Do estimate meal CAD for itinerary only.\n",
		dest, days, dates, trip.Origin, formatKnownPrefs(trip.Participants), request, foodBudgetHint(trip, days), string(factsJSON))
	slog.Info("calling gemini", "stage", "itinerary", "from", from, "text", clipLog(request, 80))
	out, err := b.LLM.Structured(ctx, prompts.ItinerarySystem(b.Config.BotName, b.today().Format("2006-01-02")),
		[]llm.Message{{Role: "user", Content: user}}, toSchema(prompts.DayItinerary))
	if err != nil {
		return trip, nil, err
	}
	itin := trip.Itinerary
	if itin == nil {
		itin = map[string]any{}
	}
	itin["advisor"] = out
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{
		"itinerary":       itin,
		"duration_nights": days,
		"destination":     dest,
	})
	if err != nil {
		return trip, nil, err
	}
	if updated != nil {
		trip = updated
	}
	return trip, out, nil
}

func (b *Brain) writeAdvisorItinerary(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	dest := tripDestination(trip)
	if dest == "" {
		return b.say(ctx, trip.GroupID, "Which city should I write the days for? Once I know that I can lay out the week.", nil)
	}
	trip, out, err := b.generateAdvisorItinerary(ctx, trip, m.Text, m.SenderName)
	if err != nil {
		return err
	}
	if out == nil {
		return b.say(ctx, trip.GroupID, "Which city should I write the days for? Once I know that I can lay out the week.", nil)
	}
	text := formatting.AdvisorItinerary(out)
	if lock := formatting.LockedTravel(trip); lock != "" {
		text = lock + "\n\n" + text
	}
	return b.say(ctx, trip.GroupID, text, nil)
}

func (b *Brain) writeRestaurantPlan(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	dest := tripDestination(trip)
	if dest == "" {
		return b.say(ctx, trip.GroupID, "Which city are we eating in? Once I know that I can pick dinner spots.", nil)
	}
	factsJSON, _ := json.Marshal(tripFacts(trip))
	user := fmt.Sprintf("City: %s\nDates: %s\nTastes: %s\nCulinary note: %s\nRequest: %s\n\n%s\nLOCKED FACTS:\n%s\n",
		dest, formatting.Dates(trip.EmbarkingDate, trip.ReturningDate), formatKnownPrefs(trip.Participants),
		trip.CulinaryDescription, m.Text, foodBudgetHint(trip, tripNights(trip, m.Text)), string(factsJSON))
	slog.Info("calling gemini", "stage", "restaurants", "from", m.SenderName, "text", clipLog(m.Text, 80))
	out, err := b.LLM.Structured(ctx, prompts.RestaurantSystem(b.Config.BotName, dest),
		[]llm.Message{{Role: "user", Content: user}}, toSchema(prompts.RestaurantPicks))
	if err != nil {
		return err
	}
	text, maps := formatting.RestaurantPlan(out, dest)
	itin := trip.Itinerary
	if itin == nil {
		itin = map[string]any{}
	}
	itin["restaurants"] = out
	if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"itinerary": itin}); err != nil {
		return err
	}
	if err := b.say(ctx, trip.GroupID, text, nil); err != nil {
		return err
	}
	if maps != "" {
		return b.say(ctx, trip.GroupID, maps, nil)
	}
	return nil
}

// ------------------------------------------------------------------ stage: search

func planGaps(trip *models.Trip) []string {
	if trip == nil {
		return []string{"who's going"}
	}
	var gaps []string
	if len(trip.Participants) == 0 {
		gaps = append(gaps, "who's going")
	}
	if originAirportCode(trip.Origin, trip.Participants) == "" {
		gaps = append(gaps, "where you're flying from")
	}
	if strings.TrimSpace(tripDestination(trip)) == "" {
		gaps = append(gaps, "the destination")
	}
	if strings.TrimSpace(trip.EmbarkingDate) == "" || strings.TrimSpace(trip.ReturningDate) == "" {
		gaps = append(gaps, "travel dates")
	}
	return gaps
}

func (b *Brain) offerPlanFinalize(ctx context.Context, trip *models.Trip, hint string) error {
	trip = b.syncTripBasics(ctx, trip, hint)
	if trip.State == models.Collecting {
		updated, err := store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil)
		if err != nil {
			return err
		}
		if updated != nil {
			trip = updated
		}
	}
	if trip.Intake != nil && !searchEssentials(trip) {
		if trip.State != models.Collecting {
			updated, err := store.SetState(ctx, b.Store, trip.ID, models.Collecting, nil)
			if err != nil {
				return err
			}
			if updated != nil {
				trip = updated
			}
		}
		return b.continueIntake(ctx, trip, nil)
	}
	if len(planGaps(trip)) == 0 && (looksLikeDashboardAsk(hint) || looksLikeSearchAsk(hint) || approveOnlyRe.MatchString(hint) || strings.Contains(hint, "✅")) {
		return b.startTravelSearch(ctx, trip)
	}
	if len(planGaps(trip)) > 0 {
		if trip.State != models.Collecting {
			updated, err := store.SetState(ctx, b.Store, trip.ID, models.Collecting, nil)
			if err != nil {
				return err
			}
			if updated != nil {
				trip = updated
			}
		}
		return b.continueIntake(ctx, trip, nil)
	}
	trip, _, err := b.generateAdvisorItinerary(ctx, trip, "Write the full day-by-day plan for this group.", "group")
	if err != nil {
		return err
	}
	if err := b.say(ctx, trip.GroupID, formatting.PlanConfirm(trip), nil); err != nil {
		return err
	}
	return b.sendPoll(ctx, trip.GroupID, "Finalize this plan?", []string{"Yes, search flights and hotels", "Not yet"}, "finalize", trip.ID)
}

func (b *Brain) syncTripBasics(ctx context.Context, trip *models.Trip, hint string) *models.Trip {
	if trip == nil {
		return trip
	}
	fields := map[string]any{}
	origin := originAirportCode(firstNonEmpty(trip.Origin, hint), trip.Participants)
	if origin == "" {
		h := harvestText(hint, b.today())
		origin = originAirportCode(strings.TrimSpace(h.Airport+" "+h.City), nil)
	}
	if origin != "" && !strings.EqualFold(trip.Origin, origin) {
		fields["origin"] = origin
		trip.Origin = origin
		for i := range trip.Participants {
			if trip.Participants[i].OriginAirport == "" {
				trip.Participants[i].OriginAirport = origin
				if trip.Participants[i].Intake != nil && !trip.Participants[i].Intake.Origin.Known() {
					trip.Participants[i].Intake.Origin = models.FieldValue{Value: origin, Confidence: models.Confirmed, UpdatedAt: models.Now()}
				}
				fields["participants"] = trip.Participants
				break
			}
		}
	}
	if trip.Intake != nil {
		if strings.TrimSpace(trip.Destination) == "" && trip.Intake.Destination.Known() {
			fields["destination"] = trip.Intake.Destination.AsString()
			trip.Destination = trip.Intake.Destination.AsString()
		}
		if trip.Intake.ExactDates != nil && strings.TrimSpace(trip.EmbarkingDate) == "" {
			fields["embarking_date"] = trip.Intake.ExactDates.Depart
			fields["returning_date"] = trip.Intake.ExactDates.Return
			trip.EmbarkingDate = trip.Intake.ExactDates.Depart
			trip.ReturningDate = trip.Intake.ExactDates.Return
		}
	}
	if len(fields) == 0 {
		return trip
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, fields)
	if err != nil || updated == nil {
		return trip
	}
	return updated
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// selectOption flattens the chosen Option onto the trip, then sends a full plan summary
// and waits for the group to finalize before any live flight/hotel search.
func (b *Brain) lockChosenOption(ctx context.Context, trip *models.Trip, number int) (*models.Trip, error) {
	var option *models.Option
	for i := range trip.Options {
		if trip.Options[i].Position == number {
			option = &trip.Options[i]
			break
		}
	}
	if option == nil {
		return trip, fmt.Errorf("unknown option %d", number)
	}
	if option.DestinationAirport == "" {
		if cands := destAirportCandidates("", option.Destination); len(cands) > 0 {
			option.DestinationAirport = cands[0]
		}
	}
	if option.EmbarkingDate == "" || option.ReturningDate == "" {
		dates := allStoredDates(trip.Participants)
		if len(dates) >= 2 {
			option.EmbarkingDate = dates[0]
			option.ReturningDate = dates[len(dates)-1]
		} else if trip.EmbarkingDate != "" && trip.ReturningDate != "" {
			option.EmbarkingDate = trip.EmbarkingDate
			option.ReturningDate = trip.ReturningDate
		} else {
			start, end := comingWeekend(b.today())
			option.EmbarkingDate = start.Format("2006-01-02")
			option.ReturningDate = end.Format("2006-01-02")
		}
	}
	if option.DurationNights == 0 {
		option.DurationNights = tripNights(trip, "")
	}
	itin := map[string]any{}
	if trip.Itinerary != nil {
		if restaurants, ok := trip.Itinerary["restaurants"]; ok {
			itin["restaurants"] = restaurants
		}
		if advisor, ok := trip.Itinerary["advisor"]; ok {
			itin["advisor"] = advisor
		}
	}
	fields := map[string]any{
		"chosen_option_position": option.Position,
		"destination":            option.Destination, "destination_airport": option.DestinationAirport,
		"activity_description": option.ActivityDescription, "culinary_description": option.CulinaryDescription,
		"duration_nights": option.DurationNights,
		"embarking_date":  option.EmbarkingDate, "returning_date": option.ReturningDate,
		"shared_dashboard": false, "itinerary": itin,
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, fields)
	if err != nil {
		return trip, err
	}
	if updated != nil {
		return updated, nil
	}
	return trip, nil
}

func (b *Brain) selectOption(ctx context.Context, trip *models.Trip, number int) error {
	trip, err := b.lockChosenOption(ctx, trip, number)
	if err != nil {
		return b.say(ctx, trip.GroupID, fmt.Sprintf("I only put up 1–%d. Which of those?", len(trip.Options)), nil)
	}
	return b.offerPlanFinalize(ctx, trip, "")
}

func (b *Brain) startTravelSearch(ctx context.Context, trip *models.Trip) error {
	if trip.Intake != nil && !searchEssentials(trip) {
		if trip.State != models.Collecting {
			updated, err := store.SetState(ctx, b.Store, trip.ID, models.Collecting, nil)
			if err != nil {
				return err
			}
			if updated != nil {
				trip = updated
			}
		}
		return b.continueIntake(ctx, trip, nil)
	}
	option := trip.ChosenOption()
	if option == nil {
		if trip.Intake != nil {
			return b.continueIntake(ctx, trip, nil)
		}
		return b.say(ctx, trip.GroupID, "Pick a destination first, then I'll send the plan to finalize.", nil)
	}
	if option.DestinationAirport == "" {
		if cands := destAirportCandidates("", option.Destination); len(cands) > 0 {
			option.DestinationAirport = cands[0]
		}
	}
	if !isoDateOK(option.EmbarkingDate) || !isoDateOK(option.ReturningDate) {
		if trip.Intake != nil && trip.Intake.ExactDates != nil && isoDateOK(trip.Intake.ExactDates.Depart) && isoDateOK(trip.Intake.ExactDates.Return) {
			option.EmbarkingDate = trip.Intake.ExactDates.Depart
			option.ReturningDate = trip.Intake.ExactDates.Return
		}
	}
	if !isoDateOK(option.EmbarkingDate) || !isoDateOK(option.ReturningDate) {
		if trip.Intake != nil {
			if trip.State != models.Collecting {
				updated, err := store.SetState(ctx, b.Store, trip.ID, models.Collecting, nil)
				if err != nil {
					return err
				}
				if updated != nil {
					trip = updated
				}
			}
			return b.continueIntake(ctx, trip, nil)
		}
		dates := allStoredDates(trip.Participants)
		if len(dates) >= 2 {
			option.EmbarkingDate = dates[0]
			option.ReturningDate = dates[len(dates)-1]
		} else {
			start, end := comingWeekend(b.today())
			option.EmbarkingDate = start.Format("2006-01-02")
			option.ReturningDate = end.Format("2006-01-02")
		}
	}
	if option.DurationNights == 0 {
		option.DurationNights = tripNights(trip, "")
	}
	trip, err := store.SetState(ctx, b.Store, trip.ID, models.Searching, nil)
	if err != nil {
		return err
	}
	sessionID := ""
	if b.Dashboard != nil {
		sessionID, err = b.Dashboard.BeginLive(ctx, trip)
		if err != nil {
			return err
		}
	}
	if err := b.say(ctx, trip.GroupID, fmt.Sprintf("%s is locked. Searching real flights and stays now.", option.Destination), nil); err != nil {
		return err
	}
	if err := b.shareLiveSearch(ctx, trip, sessionID); err != nil {
		return err
	}

	people := trip.Participants
	originAirport := originAirportCode(trip.Origin, people)

	offers, hotels, flightErr, hotelErr := b.searchDashboard(ctx, trip, originAirport, option)
	if flightErr != nil || len(offers) == 0 || hotelErr != nil || len(hotels) == 0 {
		message := "No matching flight and hotel combination was found. Choose another option in the group chat."
		if flightErr != nil || hotelErr != nil {
			message = "A travel search could not finish. Please try another option in the group chat."
		}
		if err := b.dashboardEvent(ctx, trip, "session.failed", map[string]any{"message": message}); err != nil {
			return err
		}
		if _, err := store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil); err != nil {
			return err
		}
		return b.say(ctx, trip.GroupID, message, nil)
	}
	if err := b.dashboardEvent(ctx, trip, "planning.started", map[string]any{"message": "Building your itinerary"}); err != nil {
		return err
	}
	if err := b.dashboardTask(ctx, trip, "flight-prices", "running", "Comparing flight prices"); err != nil {
		return err
	}
	if err := b.dashboardTask(ctx, trip, "hotel-location", "running", "Choosing a hotel in your destination"); err != nil {
		return err
	}
	offer, hotel, selectionReason, err := b.selectTravelPlan(ctx, trip, offers, hotels)
	if err != nil {
		message := "Travel options arrived, but the trip planner could not select a plan. Please choose the option again to retry."
		if eventErr := b.dashboardEvent(ctx, trip, "session.failed", map[string]any{"message": message}); eventErr != nil {
			return eventErr
		}
		if _, stateErr := store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil); stateErr != nil {
			return stateErr
		}
		return b.say(ctx, trip.GroupID, message, nil)
	}
	if err := b.dashboardTask(ctx, trip, "flight-prices", "completed", "Selected the flight that best fits the group’s plan"); err != nil {
		return err
	}
	if err := b.dashboardTask(ctx, trip, "hotel-location", "completed", "Selected a stay to match the group’s preferences and budget"); err != nil {
		return err
	}
	if err := b.dashboardTask(ctx, trip, "group-budget", "running", "Calculating the group’s costs"); err != nil {
		return err
	}

	names := make([]string, len(people))
	payer := "the group"
	for i, p := range people {
		names[i] = p.WhatsAppName
		if i == 0 {
			payer = p.WhatsAppName
		}
	}
	preview := tools.ComputeSplit(names, offer.Price, hotel.TotalPrice, payer)

	embarkOffer := offer
	returnOffer := offer.ReturningOffer()

	itinMap, err := structToMap(map[string]any{
		"flights": map[string]any{
			"round_trip_each": offer.Price,
			"embarking":       embarkOffer,
			"returning":       returnOffer,
		},
		"hotel":              hotel,
		"per_person":         preview.PerPerson,
		"group_total":        preview.GroupTotal,
		"flight_options":     offers,
		"hotel_options":      hotels,
		"selected_flight_id": offer.OfferID,
		"selected_hotel_id":  hotel.OfferID,
		"selection_reason":   selectionReason,
	})
	if err != nil {
		return err
	}
	keepItineraryExtras(itinMap, trip.Itinerary)
	each := preview.GroupTotal
	if spend := formatting.ComputeSpend(&models.Trip{Itinerary: itinMap, Participants: people}); spend.Ok() {
		each = spend.TravelEach
		itinMap["group_total"] = spend.TravelGroup
	}

	ed, _ := models.ParseDate(option.EmbarkingDate)
	rd, _ := models.ParseDate(option.ReturningDate)
	half := offer.Price / 2
	flights := []models.Flight{
		{Direction: models.Embarking, BookingStatus: models.StatusIncomplete, DepartingDate: ed, ArrivalDate: ed, Costs: &half, Source: offer.Source, BookingURL: offer.BookingURL},
		{Direction: models.Returning, BookingStatus: models.StatusIncomplete, DepartingDate: rd, ArrivalDate: rd, Costs: &half, Source: offer.Source, BookingURL: offer.BookingURL},
	}
	accommodations := []models.Accommodation{{
		BookingStatus: models.StatusIncomplete, CheckInDate: ed, CheckOutDate: rd,
		Rating: hotel.Rating, Costs: &hotel.TotalPrice, BookingURL: hotel.CheckoutURL,
		Source: hotel.Source, PropertyType: hotel.PropertyType, OriginalRating: hotel.OriginalRating,
		OriginalRatingScale: hotel.OriginalRatingScale, PriceNote: hotel.PriceNote,
	}}

	opts := append([]models.Option(nil), trip.Options...)
	for i := range opts {
		if opts[i].Position == option.Position {
			cp := each
			opts[i].CostPerPerson = &cp
			opts[i].DestinationAirport = offer.Destination
		}
	}

	trip, err = store.SetState(ctx, b.Store, trip.ID, models.AwaitingApproval, map[string]any{
		"itinerary": itinMap, "flights": flights, "accommodations": accommodations,
		"cost_per_person": each, "options": opts, "destination_airport": offer.Destination,
		"flights_locked": true,
	})
	if err != nil {
		return err
	}
	if err := b.finishDashboard(ctx, trip, offer, hotel); err != nil {
		message := "Your flight and hotel were selected, but activity planning could not finish. Choose the option again to retry."
		if eventErr := b.dashboardEvent(ctx, trip, "session.failed", map[string]any{"message": message}); eventErr != nil {
			return eventErr
		}
		if _, stateErr := store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, map[string]any{"flights_locked": false}); stateErr != nil {
			return stateErr
		}
		return b.say(ctx, trip.GroupID, message, nil)
	}
	chosen := trip.ChosenOption()
	return b.postSummary(ctx, trip, *chosen, itinMap, people)
}

func keepItineraryExtras(dst, src map[string]any) {
	if dst == nil || src == nil {
		return
	}
	for _, k := range []string{"restaurants", "advisor"} {
		if v, ok := src[k]; ok && v != nil {
			dst[k] = v
		}
	}
}

func destAirportCandidates(airport, dest string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(code string) {
		code = strings.ToUpper(strings.TrimSpace(code))
		if code == "" || seen[code] {
			return
		}
		seen[code] = true
		out = append(out, code)
	}
	add(airport)
	blob := strings.ToLower(dest + " " + airport)
	if strings.Contains(blob, "tel aviv") || strings.Contains(blob, "israel") || airport == "TLV" {
		add("TLV")
	}
	if strings.Contains(blob, "lisbon") || airport == "LIS" {
		add("LIS")
	}
	if strings.Contains(blob, "ital") || strings.Contains(blob, "amalfi") || strings.Contains(blob, "naples") || strings.Contains(blob, "puglia") || airport == "NAP" || airport == "BRI" {
		add("NAP")
		add("FCO")
		add("PSA")
		add("BRI")
	}
	return out
}

func originAirportCode(origin string, people []models.Participant) string {
	for _, p := range people {
		if code := airportFromLabel(p.OriginAirport); code != "" {
			return code
		}
		if code := airportFromLabel(p.Origin); code != "" {
			return code
		}
		if code := airportFromLabel(p.OriginCity); code != "" {
			return code
		}
		if p.Intake != nil {
			if code := airportFromLabel(p.Intake.Origin.AsString()); code != "" {
				return code
			}
		}
	}
	return airportFromLabel(origin)
}

func airportFromLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	up := strings.ToUpper(s)
	low := strings.ToLower(s)
	for city, code := range cityAirport {
		if strings.Contains(low, city) || strings.Contains(up, code) {
			return code
		}
	}
	if len(s) == 3 {
		return up
	}
	return ""
}

func (b *Brain) offerCheaperFlights(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	if trip.State == models.AwaitingChoice && trip.Itinerary == nil {
		return b.plan(ctx, trip, m.Text, m)
	}
	option := trip.ChosenOption()
	if option == nil && len(trip.Options) > 0 {
		option = &trip.Options[0]
	}
	if option == nil {
		return b.plan(ctx, trip, m.Text, m)
	}
	people := trip.Participants
	origin := trip.Origin
	if len(people) > 0 && people[0].OriginAirport != "" {
		origin = people[0].OriginAirport
	}
	var best *models.FlightOffer
	for _, destAP := range destAirportCandidates(option.DestinationAirport, option.Destination) {
		offers, err := tools.SearchFlights(ctx, b.Config, origin, destAP, option.EmbarkingDate, option.ReturningDate)
		if err != nil || len(offers) == 0 {
			continue
		}
		if best == nil || offers[0].Price < best.Price {
			o := offers[0]
			best = &o
		}
	}
	if best == nil {
		return b.say(ctx, trip.GroupID, "I only have the fares I already quoted. I can look at another city if you want.", nil)
	}

	hotel, err := b.hotelForTrip(ctx, trip)
	if err != nil || hotel == nil {
		hotels, hErr := tools.SearchHotels(ctx, b.Config, option.Destination, option.EmbarkingDate, option.ReturningDate, len(people), nil)
		if hErr != nil || len(hotels) == 0 {
			return b.say(ctx, trip.GroupID, "Couldn't refresh the stay with those fares.", nil)
		}
		hotel = &hotels[0]
	}

	names := make([]string, len(people))
	payer := "the group"
	for i, p := range people {
		names[i] = p.WhatsAppName
		if i == 0 {
			payer = p.WhatsAppName
		}
	}
	preview := tools.ComputeSplit(names, best.Price, hotel.TotalPrice, payer)
	embark := *best
	ret := *best
	ret.Origin, ret.Destination = best.Destination, best.Origin
	itinMap, err := structToMap(map[string]any{
		"flights": map[string]any{
			"round_trip_each": best.Price,
			"embarking":       embark,
			"returning":       ret,
		},
		"hotel":       *hotel,
		"per_person":  preview.PerPerson,
		"group_total": preview.GroupTotal,
	})
	if err != nil {
		return err
	}
	keepItineraryExtras(itinMap, trip.Itinerary)
	each := preview.GroupTotal
	if spend := formatting.ComputeSpend(&models.Trip{Itinerary: itinMap, Participants: people}); spend.Ok() {
		each = spend.TravelEach
		itinMap["group_total"] = spend.TravelGroup
	}
	opts := append([]models.Option(nil), trip.Options...)
	for i := range opts {
		if option != nil && opts[i].Position == option.Position {
			cp := each
			opts[i].CostPerPerson = &cp
			opts[i].DestinationAirport = best.Destination
		}
	}
	ed, _ := models.ParseDate(option.EmbarkingDate)
	rd, _ := models.ParseDate(option.ReturningDate)
	half := best.Price / 2
	trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{
		"itinerary": itinMap, "options": opts, "cost_per_person": each,
		"destination_airport": best.Destination, "flights_locked": true,
		"flights": []models.Flight{
			{Direction: models.Embarking, BookingStatus: models.StatusIncomplete, DepartingDate: ed, ArrivalDate: ed, Costs: &half},
			{Direction: models.Returning, BookingStatus: models.StatusIncomplete, DepartingDate: rd, ArrivalDate: rd, Costs: &half},
		},
	})
	if err != nil {
		return err
	}
	if trip.State != models.AwaitingApproval {
		trip, err = store.SetState(ctx, b.Store, trip.ID, models.AwaitingApproval, nil)
		if err != nil {
			return err
		}
	}
	intro := fmt.Sprintf("Cheaper inbound I can actually quote: %s to %s.", best.Origin, best.Destination)
	if err := b.say(ctx, trip.GroupID, intro, nil); err != nil {
		return err
	}
	chosen := trip.ChosenOption()
	if chosen == nil {
		chosen = option
	}
	return b.postSummary(ctx, trip, *chosen, itinMap, people)
}

// ------------------------------------------------------------------ stage: book

func (b *Brain) book(ctx context.Context, trip *models.Trip, approver string) error {
	trip, err := store.SetState(ctx, b.Store, trip.ID, models.BookingState, map[string]any{
		"approved_by": approver, "approved_at": models.Now(),
	})
	if err != nil {
		return err
	}
	if err := state.AssertCanBook(trip); err != nil {
		return err
	}

	people := trip.Participants
	option := trip.ChosenOption()
	if option == nil {
		return fmt.Errorf("trip %s has no chosen option", trip.ID)
	}
	itin := trip.Itinerary
	if err := b.say(ctx, trip.GroupID, "On it — booking now.", nil); err != nil {
		return err
	}
	_ = b.dashboardEvent(ctx, trip, "booking.started", map[string]any{"message": "Booking flights and the stay"})
	if err := b.dashboardTask(ctx, trip, "flight-prices", "running", "Booking the selected flights"); err != nil {
		return err
	}

	// Flights: skip legs already booked (makes ✅-to-retry safe after a hotel failure).
	flights := append([]models.Flight(nil), trip.Flights...)
	flightsRaw, _ := itin["flights"].(map[string]any)
	for i := range flights {
		if flights[i].BookingStatus == models.StatusBooked {
			continue
		}
		raw, ok := flightsRaw[string(flights[i].Direction)]
		if !ok {
			continue
		}
		var offer models.FlightOffer
		if err := decodeInto(raw, &offer); err != nil {
			return err
		}
		fb, err := tools.BookFlight(ctx, b.Config, offer, approver)
		if err != nil {
			return err
		}
		flights[i].Source, flights[i].BookingURL = offer.Source, offer.BookingURL
		flights[i].BookingStatus = models.StatusBooked
		flights[i].ProviderRef = fb.PNR
		// fb.Price is the round-trip offer's per-person price (the same offer backs both legs);
		// halve it so Costs summed across both legs still equals one round-trip price per person.
		cost := fb.Price / 2
		flights[i].Costs = &cost
	}
	if trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"flights": flights}); err != nil {
		return err
	}
	_ = b.dashboardTask(ctx, trip, "flight-prices", "completed", "Flights booked")
	_ = b.dashboardTask(ctx, trip, "hotel-location", "running", "Booking the stay")

	// Hotel: Skyvern (or mock). Starts instantly, then we wait for the browser to finish.
	var hotel models.HotelOffer
	if err := decodeInto(itin["hotel"], &hotel); err != nil {
		return err
	}
	accommodations := append([]models.Accommodation(nil), trip.Accommodations...)
	if len(accommodations) == 0 {
		ci, _ := models.ParseDate(option.EmbarkingDate)
		co, _ := models.ParseDate(option.ReturningDate)
		accommodations = []models.Accommodation{{
			BookingStatus: models.StatusIncomplete, CheckInDate: ci, CheckOutDate: co,
			Rating: hotel.Rating, Costs: &hotel.TotalPrice, BookingURL: hotel.CheckoutURL,
			Source: hotel.Source, PropertyType: hotel.PropertyType, OriginalRating: hotel.OriginalRating,
			OriginalRatingScale: hotel.OriginalRatingScale, PriceNote: hotel.PriceNote,
		}}
	}
	acc := &accommodations[0]

	if acc.BookingStatus != models.StatusBooked {
		runID, liveURL, err := tools.StartHotelBooking(ctx, b.Config, hotel, len(people), approver, defaultLeadEmail,
			fmt.Sprintf("Trip %s hotel", trip.ID))
		if err != nil {
			return err
		}
		if liveURL != "" && !isLocalOrFakeURL(liveURL) {
			if err := b.say(ctx, trip.GroupID, "Hotel's going through now.", nil); err != nil {
				return err
			}
		}

		result, err := tools.WaitForBooking(ctx, b.Config, runID, 3*time.Second, 600*time.Second)
		if err != nil {
			return err
		}
		if result.Status != "completed" || result.ConfirmationNumber == "" {
			if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"accommodations": accommodations}); err != nil {
				return err
			}
			if _, err := store.SetState(ctx, b.Store, trip.ID, models.AwaitingApproval, nil); err != nil {
				return err
			}
			return b.say(ctx, trip.GroupID, "Flights are in, but the hotel didn't go through. Say yes and I'll retry the hotel.", nil)
		}

		cost := hotel.TotalPrice
		if result.TotalPrice != nil {
			cost = *result.TotalPrice
		}
		acc.BookingStatus = models.StatusBooked
		acc.ProviderRef = result.ConfirmationNumber
		acc.Costs = &cost
	}
	if trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"accommodations": accommodations}); err != nil {
		return err
	}

	payer := strings.TrimSpace(trip.PayerName)
	if match := matchName(payer, people); match != nil {
		payer = match.WhatsAppName
	} else if designated := designatedPayer(people); designated != "" {
		payer = designated
	} else if match := matchName(approver, people); match != nil {
		payer = match.WhatsAppName
	} else if len(people) > 0 {
		payer = people[0].WhatsAppName
	} else if payer == "" {
		payer = approver
	}
	names := make([]string, len(people))
	for i, p := range people {
		names[i] = p.WhatsAppName
	}
	var flightPricePerPerson float64
	for _, f := range flights {
		if f.Costs != nil {
			flightPricePerPerson += *f.Costs
		}
	}
	hotelCost := 0.0
	if acc.Costs != nil {
		hotelCost = *acc.Costs
	}
	split := tools.ComputeSplit(names, flightPricePerPerson, hotelCost, payer)
	splitMap, err := structToMap(split)
	if err != nil {
		return err
	}
	newItin := make(map[string]any, len(itin)+1)
	for k, v := range itin {
		newItin[k] = v
	}
	newItin["split"] = splitMap
	if _, err := store.SetState(ctx, b.Store, trip.ID, models.Booked, map[string]any{"itinerary": newItin}); err != nil {
		return err
	}
	_ = b.dashboardTask(ctx, trip, "hotel-location", "completed", "Stay booked")
	_ = b.dashboardEvent(ctx, trip, "session.completed", map[string]any{"message": "Flights and stay booked"})

	var embarkRef, returnRef string
	for _, f := range flights {
		switch f.Direction {
		case models.Embarking:
			embarkRef = f.ProviderRef
		case models.Returning:
			returnRef = f.ProviderRef
		}
	}
	return b.say(ctx, trip.GroupID, formatting.ConfirmationMessage(option.Destination, embarkRef, returnRef, acc.ProviderRef, split), nil)
	// TODO(P4, hour 12-15): trigger the ElevenLabs hotel call here and post its result.
}

// ------------------------------------------------------------------ outbound

func (b *Brain) say(ctx context.Context, groupID, text string, buttons []messaging.Button) error {
	text = scrubWhatsAppIDs(text)
	if err := b.Messenger.Send(ctx, groupID, text, buttons); err != nil {
		return err
	}
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil {
		return err
	}
	tripID := ""
	if trip != nil {
		tripID = trip.ID
	}
	_, err = b.Store.SaveMessage(ctx, &models.Message{
		GroupID: groupID, TripID: tripID, SenderID: "bot", SenderName: b.Config.BotName,
		Text: text, IsBot: true, SentAt: models.Now(),
	})
	return err
}

// ------------------------------------------------------------------ helpers

func compactChat(history []models.Message, n int) string {
	if n <= 0 || len(history) == 0 {
		return ""
	}
	if len(history) > n {
		history = history[len(history)-n:]
	}
	lines := make([]string, 0, len(history))
	for _, msg := range history {
		who := msg.SenderName
		if msg.IsBot {
			who = "Fare"
		}
		text := strings.TrimSpace(msg.Text)
		if len(text) > 180 {
			text = text[:180] + "…"
		}
		if text == "" {
			continue
		}
		lines = append(lines, who+": "+text)
	}
	return strings.Join(lines, "\n")
}

func clipLog(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if n > 0 && len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func optionButtons(options []models.Option) []messaging.Button {
	buttons := make([]messaging.Button, len(options))
	for i, o := range options {
		label := strconv.Itoa(o.Position)
		buttons[i] = messaging.Button{Label: label, Payload: label}
	}
	return buttons
}

// toSchema adapts a prompts.go {"name":..., "schema":...} map into llm.Schema.
func toSchema(m map[string]any) llm.Schema {
	name, _ := m["name"].(string)
	schema, _ := m["schema"].(map[string]any)
	return llm.Schema{Name: name, Schema: schema}
}

// decodeInto round-trips through JSON to decode a loosely-typed value (map[string]any / []any,
// from Gemini's structured output) into a concrete Go type.
func decodeInto[T any](v any, out *T) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func structToMap(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}
