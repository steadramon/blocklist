package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	sourceURLValidatorMap = map[string]lineValidator{
		`https://raw.githubusercontent.com/notracking/hosts-blocklists/master/hostnames.txt`:         hostLine("0.0.0.0"),
		`http://dn-mwsl-hosts.qbox.me/hosts`:                                                         hostLine("191.101.231.96"),
		`https://adaway.org/hosts.txt`:                                                               hostLine("127.0.0.1"),
		`http://sysctl.org/cameleon/hosts`:                                                           hostLine("127.0.0.1"),
		`http://www.hostsfile.org/Downloads/hosts.txt`:                                               hostLine("127.0.0.1"),
		`https://raw.githubusercontent.com/yous/YousList/master/hosts.txt`:                           hostLine("0.0.0.0"),
		`https://download.dnscrypt.info/blacklists/domains/mybase.txt`:                               domainListLine(),
		`https://raw.githubusercontent.com/koala0529/adhost/master/adhosts`:                          hostLine("127.0.0.1"),
		`http://mirror1.malwaredomains.com/files/justdomains`:                                        domainListLine(),
		`http://ransomwaretracker.abuse.ch/downloads/RW_DOMBL.txt`:                                   domainListLine(),
		`https://s3.amazonaws.com/lists.disconnect.me/simple_tracking.txt`:                           domainListLine(),
		`https://raw.githubusercontent.com/azet12/KADhosts/master/KADhosts.txt`:                      hostLine("0.0.0.0"),
		`https://raw.githubusercontent.com/lack006/Android-Hosts-L/master/hosts_files/2016_hosts/AD`: hostLine("127.0.0.1"),
		`https://gitlab.com/ZeroDot1/CoinBlockerLists/raw/master/hosts`:                              hostLine("0.0.0.0"),
	}
	shortURLs = []string{
		`db.tt`,
		`www.db.tt`,
		`j.mp`,
		`www.j.mp`,
		`bit.ly`,
		`www.bit.ly`,
		`pix.bit.ly`,
		`goo.gl`,
		`www.goo.gl`,
	}
	whitelist = []whitelistChecker{
		contains(`google-analytics`),
		suffix(`msedge.net`),
		equal(`amazonaws.com`),
		equal(`mp.weixin.qq.com`),
		equal(`url.cn`),
		regex(`^s3[\d\w\-]*.amazonaws.com`),
		suffix(`internetdownloadmanager.com`),
		suffix(`.alcohol-soft.com`),
		equal(`scootersoftware.com`),
		regex(`[^ad]\.mail\.ru`),
		regex(`[^ad]\.daum\.net`),
		regex(`^\w{1,10}\.yandex\.`),
		suffix(`.googlevideo.com`),
		regex(`^[^\.]+\.elb\.amazonaws\.com`),
		suffix(`.in-addr.arpa`),
		suffix(`.url.cn`),
		equal(`qq.com`),
		equal(`www.qq.com`),
		equal(`analytics.163.com`),
		equal(`163.com`),
		equal(`behance.net`),
		suffix(`.verisign.com`),
		contains(`mozilla`),
	}
	tlds               = make(map[string]struct{})
	tldsMutex          sync.Mutex
	effectiveTLDsNames []string
	mutex              sync.Mutex
	sema               = newSemaphore(50)
	finalDomains       = make(map[string]struct{})
	blockDomain        = make(chan string, 20)
	quit               = make(chan bool)
)

const (
	blocklist                         = `toblock.lst`
	blocklistWithoutShortURL          = `toblock-without-shorturl.lst`
	blocklistOptimized                = `toblock-optimized.lst`
	blocklistWithoutShortURLOptimized = `toblock-without-shorturl-optimized.lst`
	tldsURL                           = `http://data.iana.org/TLD/tlds-alpha-by-domain.txt`
	effectiveTLDsNamesURL             = `https://publicsuffix.org/list/effective_tld_names.dat`
)

type semaphore struct {
	c chan int
}

func newSemaphore(n int) *semaphore {
	s := &semaphore{
		c: make(chan int, n),
	}
	return s
}

func (s *semaphore) Acquire() {
	s.c <- 0
}

func (s *semaphore) Release() {
	<-s.c
}

type lineValidator func(s string) string

func hostLine(addr string) lineValidator {
	regexPattern := fmt.Sprintf(`^(%s)\s+([\w\d\-\._]+)`, strings.Replace(addr, `.`, `\.`, -1))
	validDomain := regexp.MustCompile(`^((xn--)?[\w\d]+([\w\d\-_]+)*\.)+\w{2,}$`)
	validLine := regexp.MustCompile(regexPattern)
	return func(s string) string {
		ss := validLine.FindStringSubmatch(s)
		if len(ss) > 1 {
			if validDomain.MatchString(ss[2]) {
				return ss[2]
			}
		}
		log.Println("invalid line:", s)
		return ""
	}
}

func domainListLine() lineValidator {
	validDomain := regexp.MustCompile(`^((xn--)?[\w\d]+([\w\d\-_]+)*\.)+\w{2,}$`)
	return func(s string) string {
		if validDomain.MatchString(s) {
			return s
		}
		log.Println("invalid domain:", s)
		return ""
	}
}

type whitelistChecker func(s string) bool

func contains(pattern string) whitelistChecker {
	return func(s string) bool {
		return strings.Contains(s, pattern)
	}
}

func suffix(pattern string) whitelistChecker {
	return func(s string) bool {
		return strings.HasSuffix(s, pattern)
	}
}

func prefix(pattern string) whitelistChecker {
	return func(s string) bool {
		return strings.HasPrefix(s, pattern)
	}
}

func equal(pattern string) whitelistChecker {
	return func(s string) bool {
		return pattern == s
	}
}

func regex(pattern string) whitelistChecker {
	r := regexp.MustCompile(pattern)
	return func(s string) bool {
		return r.MatchString(s)
	}
}

func downloadRemoteContent(remoteLink string) (io.ReadCloser, error) {
	response, err := http.Get(remoteLink)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	return response.Body, nil
}

func matchTLDs(domain string) bool {
	dd := strings.Split(domain, ".")
	lastSection := dd[len(dd)-1]
	if _, ok := tlds[lastSection]; ok {
		return true
	}

	for _, v := range effectiveTLDsNames {
		if strings.HasSuffix(domain, v) {
			return true
		}
	}

	return false
}

func inWhitelist(domain string) bool {
	for _, wl := range whitelist {
		if wl(domain) {
			return true
		}
	}
	return false
}

func existent(domain string) (bool, error) {
	req, err := http.NewRequest("GET", "https://dns.google.com/resolve", nil)
	if err != nil {
		log.Println("creating request failed:", domain, err)
		return true, err
	}
	q := req.URL.Query()
	q.Add("name", domain)
	req.URL.RawQuery = q.Encode()

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Println("doing request failed:", domain, err)
		return true, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Println(domain, resp.Status)
		return true, fmt.Errorf("unexpected status code:%s", resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println("reading response failed:", domain, err)
		return true, err
	}

	var response struct {
		Status int `json:"Status"`
	}
	if err = json.Unmarshal(body, &response); err != nil {
		log.Println("unmarshalling response failed:", domain, err)
		return true, err
	}

	if response.Status == 3 {
		return false, nil
	}

	return true, nil
}

func process(r io.ReadCloser, validator lineValidator) (domains []string, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		// extract valid lines
		domain := validator(strings.ToLower(scanner.Text()))
		if domain == "" {
			continue
		}

		// remove items that don't match TLDs
		if !matchTLDs(domain) {
			log.Println("don't match TLDs:", domain)
			continue
		}

		// remove items in white list
		if inWhitelist(domain) {
			log.Println("in whitelist:", domain)
			continue
		}
		domains = append(domains, domain)
	}
	r.Close()
	return
}

func saveToFile(content string, path string) error {
	file, err := os.OpenFile(path, os.O_TRUNC|os.O_WRONLY|os.O_CREATE, 0644)
	if err == nil {
		file.WriteString(content)
		file.Close()
		return nil
	}

	log.Println(err)
	return err
}

func generateTLDs(wg *sync.WaitGroup) {
	err := os.ErrNotExist
	var r io.ReadCloser
	for i := 0; i < 10 && err != nil; time.Sleep(5 * time.Second) {
		r, err = downloadRemoteContent(tldsURL)
		i++
	}
	if err == nil {
		scanner := bufio.NewScanner(r)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			tldsMutex.Lock()
			tlds[strings.ToLower(scanner.Text())] = struct{}{}
			tldsMutex.Unlock()
		}
		r.Close()
	}
	wg.Done()
}

func generateEffectiveTLDsNames(wg *sync.WaitGroup) {
	err := os.ErrNotExist
	var r io.ReadCloser
	for i := 0; i < 10 && err != nil; time.Sleep(5 * time.Second) {
		r, err = downloadRemoteContent(effectiveTLDsNamesURL)
		i++
	}
	if err == nil {
		scanner := bufio.NewScanner(r)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			line := strings.ToLower(scanner.Text())
			if len(line) == 0 {
				continue
			}
			c := line[0]
			if c >= byte('a') && c <= byte('z') || c >= byte('0') && c <= byte('9') {
				if strings.IndexByte(line, byte('.')) < 0 {
					tldsMutex.Lock()
					tlds[line] = struct{}{}
					tldsMutex.Unlock()
				} else {
					effectiveTLDsNames = append(effectiveTLDsNames, "."+line)
				}
			}
		}
		r.Close()
	}
	wg.Done()
}

func getDomains(u string, v lineValidator, domains map[string]struct{}, wg *sync.WaitGroup) {
	// download hosts
	err := os.ErrNotExist
	var r io.ReadCloser
	for i := 0; i < 10 && err != nil; time.Sleep(5 * time.Second) {
		r, err = downloadRemoteContent(u)
		i++
	}
	if err == nil {
		d, _ := process(r, v)
		for _, domain := range d {
			// so could remove duplicates
			mutex.Lock()
			domains[domain] = struct{}{}
			mutex.Unlock()
		}
	}
	wg.Done()
}

func receiveDomains() {
	for {
		select {
		case domain := <-blockDomain:
			finalDomains[domain] = struct{}{}
		case <-quit:
			return
		}
	}
}

func checkExistent(domain string, wg *sync.WaitGroup) {
	// remove items that doesn't exist actually
	for i := 0; i < 10; time.Sleep(3 * time.Second) {
		exists, err := existent(domain)
		if err != nil {
			continue
		}
		if exists {
			blockDomain <- domain
		} else {
			log.Println("google dns reports as non-exist:", domain)
		}
		break
	}
	sema.Release()
	wg.Done()
}

func main() {
	var wg sync.WaitGroup

	// generate TLDs
	wg.Add(2)
	go generateTLDs(&wg)
	go generateEffectiveTLDsNames(&wg)
	wg.Wait()

	// get blocked domain names
	domains := make(map[string]struct{})
	wg.Add(len(sourceURLValidatorMap))
	for u, v := range sourceURLValidatorMap {
		go getDomains(u, v, domains, &wg)
	}
	wg.Wait()

	// remove non-exist domain names
	go receiveDomains()
	wg.Add(len(domains))
	for domain := range domains {
		sema.Acquire()
		go checkExistent(domain, &wg)
	}
	wg.Wait()
	quit <- true

	// handle domain names of short URL services
	for _, v := range shortURLs {
		delete(finalDomains, v)
	}
	d := make([]string, len(finalDomains))
	i := 0
	for k := range finalDomains {
		d[i] = k
		i++
	}
	// save to file in order
	sort.Strings(d)
	c := strings.Join(d, "\n")
	saveToFile(c, blocklistWithoutShortURL)
	d = append(d, shortURLs...)
	sort.Strings(d)
	c = strings.Join(d, "\n")
	saveToFile(c, blocklist)
	// optimized
	d = make([]string, len(finalDomains))
	i = 0
	for k := range finalDomains {
		pick := true
		kk := strings.Split(k, ".")
		for l := 1; l < len(kk); l++ {
			dn := strings.Join(kk[l:], ".")
			if _, ok := finalDomains[dn]; ok {
				pick = false
				break
			}
		}
		if pick {
			d[i] = k
			i++
		}
	}
	d = d[:i]
	// save to file in order
	sort.Strings(d)
	c = strings.Join(d, "\n")
	saveToFile(c, blocklistWithoutShortURLOptimized)
	d = append(d, shortURLs...)
	sort.Strings(d)
	c = strings.Join(d, "\n")
	saveToFile(c, blocklistOptimized)
}
