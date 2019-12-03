/**
 * Copyright 2019 Alexander Bezzubov.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// CLI tool for aggregating unread messages in Gmail from Google Scholar Alert.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/bzz/scholar-alert-digest/gmailutils"

	"github.com/antchfx/htmlquery"
	"gitlab.com/golang-commonmark/markdown"
	"google.golang.org/api/gmail/v1"
)

const (
	labelName  = "[-oss-]-_ml-in-se" // "[ OSS ]/_ML-in-SE" in the Web UI
	scholarURL = "http://scholar.google.com/scholar_url?url="

	usageMessage = `usage: go run [-labels] [-html] [-mark] [-l <your-gmail-label>]

Polls Gmail API for unread Google Scholar alert messaged under a given label,
aggregates by paper title and prints a list of paper URLs in Markdown format.

The -labels flag will only list all available labels for the current account.
The -html flag will produce ouput report in HTML format.
The -mark flag will mark all the aggregated emails as read in Gmail"
`

	mdTemplText = `# Google Scholar Alert Digest

**Date**: {{.Date}}
**Unread emails**: {{.UnreadEmails}}
**Paper titles**: {{.TotalPapers}}
**Uniq paper titles**: {{.UniqPapers}}

{{ range $paper := sortedKeys .Papers }}
 - [{{ .Title }}]({{ .URL }}) ({{index $.Papers .}})
   {{- if .Abstract.Full }}
   <details>
    <summary>{{.Abstract.FirstLine}}</summary>{{.Abstract.RestLines}}
   </details>
   {{ end }}
{{ end }}
`

	htmlTemplText = `<!DOCTYPE html>
<html lang="en">
  <head><meta charset="UTF-8"></head>
  <body>%s</body>
</html>
`
)

var (
	user = "me"

	gmailLabel = flag.String("l", labelName, "name of the Gmail label")
	listLabels = flag.Bool("labels", false, "list all Gmail labels")
	// TODO(bzz): a format flag \w validated md/html options would be better
	ouputHTML = flag.Bool("html", false, "output report in HTML (instead of default Markdown)")
	markRead  = flag.Bool("mark", false, "marks all aggregated emails as read")
)

func usage() {
	fmt.Fprintf(os.Stderr, usageMessage)
	os.Exit(0)
}

func main() {
	flag.Usage = usage
	flag.Parse()

	client := gmailutils.NewClient(*markRead)
	srv, err := gmail.New(client)
	if err != nil {
		log.Fatalf("Unable to create a Gmail client: %v", err)
	}

	if *listLabels {
		gmailutils.PrintAllLabels(srv, user)
		os.Exit(0)
	}

	// TODO(bzz): fetchGmailMsgsAsync returning chan *gmail.Message
	var messages []*gmail.Message = fetchGmailMsgs(srv, user, *gmailLabel)
	errCount, titlesCount, uniqTitles := extractPapersFromMsgs(messages)

	if *ouputHTML {
		generateAndPrintHTML(mdTemplText, len(messages), titlesCount, uniqTitles)
	} else {
		generateAndPrintMarkdown(mdTemplText, len(messages), titlesCount, uniqTitles)
	}

	if *markRead {
		// TODO(bzz): add a state
		//  use existing report from FS \w a checkbox state set by the user
		//  only mark email as "read" iff all the links are checked off
		markGmailMsgsUnread(srv, user, messages)
	}

	if errCount != 0 {
		log.Printf("Errors: %d\n", errCount)
	}
}

// fetchGmailMsgs fetches all unread messages under a certain lable from Gmail.
func fetchGmailMsgs(srv *gmail.Service, user, label string) []*gmail.Message {
	start := time.Now()
	if envLabel, ok := os.LookupEnv("SAD_LABEL"); ok {
		gmailLabel = &envLabel
	}

	msgs := gmailutils.UnreadMessagesInLabel(srv, user, label)
	log.Printf("%d unread messages found (took %.0f sec)", len(msgs), time.Since(start).Seconds())
	return msgs
}

func extractPapersFromMsgs(messages []*gmail.Message) (int, int, map[paper]int) {
	errCount := 0
	titlesCount := 0
	uniqTitles := map[paper]int{}

	for _, m := range messages {
		papers, err := extractPapersFromMsg(m)
		if err != nil {
			errCount++
			continue
		}

		titlesCount += len(papers)
		for _, paper := range papers { // map title to uniqTitles
			uniqTitles[paper]++
		}
	}

	return errCount, titlesCount, uniqTitles
}

func extractPapersFromMsg(m *gmail.Message) ([]paper, error) {
	subj := gmailutils.Subject(m.Payload)

	body, err := gmailutils.MessageTextBody(m)
	if err != nil {
		e := fmt.Errorf("failed to get message text for ID %s - %s", m.Id, err)
		return nil, e
	}

	doc, err := htmlquery.Parse(bytes.NewReader(body))
	if err != nil {
		e := fmt.Errorf("failed to parse HTML body of %q", subj)
		return nil, e
	}

	// paper titles, from a single email
	xpTitle := "//h3/a"
	titles, err := htmlquery.QueryAll(doc, xpTitle)
	if err != nil {
		return nil, fmt.Errorf("title: not valid XPath expression %q", xpTitle)
	}

	// paper urls, from a single email
	xpURL := "//h3/a/@href"
	urls, err := htmlquery.QueryAll(doc, xpURL)
	if err != nil {
		return nil, fmt.Errorf("url: not valid XPath expression %q", xpURL)
	}

	if len(titles) != len(urls) {
		e := fmt.Errorf("titles %d != %d urls in %q", len(titles), len(urls), subj)
		return nil, e
	}

	// paper abstract
	xpAbs := "//h3/following-sibling::div[2]"
	abss, err := htmlquery.QueryAll(doc, xpAbs)
	if err != nil {
		return nil, fmt.Errorf("abstract: not valid XPath expression %q", xpAbs)
	}

	var papers []paper
	for i, aTitle := range titles {
		title := strings.TrimSpace(htmlquery.InnerText(aTitle))
		abs := strings.TrimSpace(htmlquery.InnerText(abss[i]))

		longURL := strings.TrimPrefix(htmlquery.InnerText(urls[i]), scholarURL)
		url, err := url.QueryUnescape(longURL[:strings.Index(longURL, "&")])
		if err != nil {
			log.Printf("Skipping paper %q in %q: %s", title, subj, err)
			continue
		}

		papers = append(papers, paper{
			title, url, abstract{
				abs, separateFirstLine(abs)[0], separateFirstLine(abs)[1],
			},
		})
	}
	return papers, nil
}

func separateFirstLine(text string) []string {
	text = strings.ReplaceAll(text, "\n", "")
	n := 80 // TODO(bzz): whitespace-aware splitting alg capped by max N
	if len(text) < n {
		return []string{text, ""}
	}
	return []string{text[:n], text[n:]}
}

func generateAndPrintHTML(tmplText string, messagesCount, titlesCount int, papers map[paper]int) {
	var mdBuf bytes.Buffer
	generateMdReport(&mdBuf, mdTemplText, messagesCount, titlesCount, papers)
	md := markdown.New(markdown.XHTMLOutput(true), markdown.HTML(true))
	fmt.Printf(htmlTemplText, md.RenderToString([]byte(mdBuf.String())))
}

func generateAndPrintMarkdown(tmplText string, messagesCount, titlesCount int, papers map[paper]int) {
	generateMdReport(os.Stdout, tmplText, messagesCount, titlesCount, papers)
}

func generateMdReport(out io.Writer, tmplText string, messagesCount, titlesCount int, papers map[paper]int) {
	tmpl := template.Must(template.New("unread-papers").Funcs(template.FuncMap{
		"sortedKeys": sortedKeys,
	}).Parse(tmplText))
	err := tmpl.Execute(out, struct {
		Date         string
		UnreadEmails int
		TotalPapers  int
		UniqPapers   int
		Papers       map[paper]int
	}{
		time.Now().Format(time.RFC3339),
		messagesCount,
		titlesCount,
		len(papers),
		papers,
	})
	if err != nil {
		log.Fatalf("template %q execution failed: %s", tmplText, err)
	}
}

func markGmailMsgsUnread(srv *gmail.Service, user string, messages []*gmail.Message) {
	const label = "UNREAD"
	var msgIds []string
	for _, msg := range messages {
		msgIds = append(msgIds, msg.Id)
	}

	err := srv.Users.Messages.BatchModify(user, &gmail.BatchModifyMessagesRequest{
		Ids:            msgIds,
		RemoveLabelIds: []string{label},
	}).Do()
	if err != nil {
		log.Printf("failed to batch-delete label %s from %d messages: %s",
			label, len(messages), err)
	}
	// TODO(bzz): move to
	//  gmailutils.ModifyMessagesDelLabel(srv, user, messages, "UNREAD")
}

// Helpers for a Map, sorted by keys.
// TODO(bzz): move to map.go after `go run main.go` is replaced by ./cmd/report
type sortedMap struct {
	m map[paper]int
	s []paper
}

func (sm *sortedMap) Len() int           { return len(sm.m) }
func (sm *sortedMap) Less(i, j int) bool { return sm.m[sm.s[i]] > sm.m[sm.s[j]] }
func (sm *sortedMap) Swap(i, j int)      { sm.s[i], sm.s[j] = sm.s[j], sm.s[i] }

// TODO(bzz): use a stable sort
func sortedKeys(m map[paper]int) []paper {
	sm := new(sortedMap)
	sm.m = m
	sm.s = make([]paper, len(m))
	i := 0
	for key := range m {
		sm.s[i] = key
		i++
	}
	sort.Sort(sm)
	return sm.s
}

type paper struct {
	Title, URL string
	Abstract   abstract
}

type abstract struct {
	Full, FirstLine, RestLines string
}
