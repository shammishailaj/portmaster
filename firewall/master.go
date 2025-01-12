package firewall

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Safing/portbase/log"
	"github.com/Safing/portbase/notifications"
	"github.com/Safing/portmaster/intel"
	"github.com/Safing/portmaster/network"
	"github.com/Safing/portmaster/network/netutils"
	"github.com/Safing/portmaster/network/packet"
	"github.com/Safing/portmaster/process"
	"github.com/Safing/portmaster/profile"
	"github.com/Safing/portmaster/status"
	"github.com/miekg/dns"

	"github.com/agext/levenshtein"
)

// Call order:
//
// 1. DecideOnCommunicationBeforeIntel (if connecting to domain)
//    is called when a DNS query is made, before the query is resolved
// 2. DecideOnCommunicationAfterIntel (if connecting to domain)
//    is called when a DNS query is made, after the query is resolved
// 3. DecideOnCommunication
//    is called when the first packet of the first link of the communication arrives
// 4. DecideOnLink
//		is called when when the first packet of a link arrives only if communication has verdict UNDECIDED or CANTSAY

// DecideOnCommunicationBeforeIntel makes a decision about a communication before the dns query is resolved and intel is gathered.
func DecideOnCommunicationBeforeIntel(comm *network.Communication, fqdn string) {

	// check if communication needs reevaluation
	if comm.NeedsReevaluation() {
		log.Infof("firewall: re-evaluating verdict on %s", comm)
		comm.ResetVerdict()
	}

	// check if need to run
	if comm.GetVerdict() != network.VerdictUndecided {
		return
	}

	// grant self
	if comm.Process().Pid == os.Getpid() {
		log.Infof("firewall: granting own communication %s", comm)
		comm.Accept("")
		return
	}

	// get and check profile set
	profileSet := comm.Process().ProfileSet()
	if profileSet == nil {
		log.Errorf("firewall: denying communication %s, no Profile Set", comm)
		comm.Deny("no Profile Set")
		return
	}
	profileSet.Update(status.ActiveSecurityLevel())

	// check for any network access
	if !profileSet.CheckFlag(profile.Internet) && !profileSet.CheckFlag(profile.LAN) {
		log.Infof("firewall: denying communication %s, accessing Internet or LAN not permitted", comm)
		comm.Deny("accessing Internet or LAN not permitted")
		return
	}

	// check endpoint list
	result, reason := profileSet.CheckEndpointDomain(fqdn)
	switch result {
	case profile.NoMatch:
		comm.UpdateVerdict(network.VerdictUndecided)
		if profileSet.GetProfileMode() == profile.Whitelist {
			log.Infof("firewall: denying communication %s, domain is not whitelisted", comm)
			comm.Deny("domain is not whitelisted")
		}
	case profile.Undeterminable:
		comm.UpdateVerdict(network.VerdictUndeterminable)
	case profile.Denied:
		log.Infof("firewall: denying communication %s, endpoint is blacklisted: %s", comm, reason)
		comm.Deny(fmt.Sprintf("endpoint is blacklisted: %s", reason))
	case profile.Permitted:
		log.Infof("firewall: permitting communication %s, endpoint is whitelisted: %s", comm, reason)
		comm.Accept(fmt.Sprintf("endpoint is whitelisted: %s", reason))
	}
}

// DecideOnCommunicationAfterIntel makes a decision about a communication after the dns query is resolved and intel is gathered.
func DecideOnCommunicationAfterIntel(comm *network.Communication, fqdn string, rrCache *intel.RRCache) {
	// rrCache may be nil, when function is called for re-evaluation by DecideOnCommunication

	// check if need to run
	if comm.GetVerdict() != network.VerdictUndecided {
		return
	}

	// grant self - should not get here
	if comm.Process().Pid == os.Getpid() {
		log.Infof("firewall: granting own communication %s", comm)
		comm.Accept("")
		return
	}

	// check if there is a profile
	profileSet := comm.Process().ProfileSet()
	if profileSet == nil {
		log.Errorf("firewall: denying communication %s, no Profile Set", comm)
		comm.Deny("no Profile Set")
		return
	}
	profileSet.Update(status.ActiveSecurityLevel())

	// TODO: Stamp integration

	switch profileSet.GetProfileMode() {
	case profile.Whitelist:
		log.Infof("firewall: denying communication %s, domain is not whitelisted", comm)
		comm.Deny("domain is not whitelisted")
		return
	case profile.Blacklist:
		log.Infof("firewall: permitting communication %s, domain is not blacklisted", comm)
		comm.Accept("domain is not blacklisted")
		return
	}

	// ProfileMode == Prompt

	// check relation
	if profileSet.CheckFlag(profile.Related) {
		if checkRelation(comm, fqdn) {
			return
		}
	}

	// prompt

	// first check if there is an existing notification for this.
	nID := fmt.Sprintf("firewall-prompt-%d-%s", comm.Process().Pid, comm.Domain)
	nTTL := 15 * time.Second
	n := notifications.Get(nID)
	if n != nil {
		// we were not here first, only get verdict, do not make changes
		select {
		case promptResponse := <-n.Response():
			switch promptResponse {
			case "permit-all", "permit-distinct":
				comm.Accept("permitted by user")
			default:
				comm.Deny("denied by user")
			}
		case <-time.After(nTTL):
			comm.SetReason("user did not respond to prompt")
		}
		return
	}

	// create new notification
	n = (&notifications.Notification{
		ID:      nID,
		Message: fmt.Sprintf("Application %s wants to connect to %s", comm.Process(), comm.Domain),
		Type:    notifications.Prompt,
		AvailableActions: []*notifications.Action{
			&notifications.Action{
				ID:   "permit-all",
				Text: fmt.Sprintf("Permit all %s", comm.Domain),
			},
			&notifications.Action{
				ID:   "permit-distinct",
				Text: fmt.Sprintf("Permit %s", comm.Domain),
			},
			&notifications.Action{
				ID:   "deny",
				Text: "Deny",
			},
		},
		Expires: time.Now().Add(nTTL).Unix(),
	}).Init().Save()

	// react
	select {
	case promptResponse := <-n.Response():
		n.Cancel()

		new := &profile.EndpointPermission{
			Type:    profile.EptDomain,
			Value:   comm.Domain,
			Permit:  true,
			Created: time.Now().Unix(),
		}

		switch promptResponse {
		case "permit-all":
			new.Value = "." + new.Value
		case "permit-distinct":
			// everything already set
		default:
			// deny
			new.Permit = false
		}

		if new.Permit {
			log.Infof("firewall: user permitted communication %s -> %s", comm.Process(), new.Value)
			comm.Accept("permitted by user")
		} else {
			log.Infof("firewall: user denied communication %s -> %s", comm.Process(), new.Value)
			comm.Deny("denied by user")
		}

		profileSet.Lock()
		defer profileSet.Unlock()
		userProfile := profileSet.UserProfile()
		userProfile.Lock()
		defer userProfile.Unlock()

		userProfile.Endpoints = append(userProfile.Endpoints, new)
		go userProfile.Save("")

	case <-time.After(nTTL):
		n.Cancel()
		comm.SetReason("user did not respond to prompt")

	}
}

// FilterDNSResponse filters a dns response according to the application profile and settings.
func FilterDNSResponse(comm *network.Communication, fqdn string, rrCache *intel.RRCache) *intel.RRCache {
	// do not modify own queries - this should not happen anyway
	if comm.Process().Pid == os.Getpid() {
		return rrCache
	}

	// check if there is a profile
	profileSet := comm.Process().ProfileSet()
	if profileSet == nil {
		log.Infof("firewall: blocking dns query of communication %s, no Profile Set", comm)
		return nil
	}
	profileSet.Update(status.ActiveSecurityLevel())

	// save config for consistency during function call
	secLevel := profileSet.SecurityLevel()
	filterByScope := filterDNSByScope(secLevel)
	filterByProfile := filterDNSByProfile(secLevel)

	// check if DNS response filtering is completely turned off
	if !filterByScope && !filterByProfile {
		return rrCache
	}

	// duplicate entry
	rrCache = rrCache.ShallowCopy()
	rrCache.FilteredEntries = make([]string, 0)

	// change information
	var addressesRemoved int
	var addressesOk int

	// loop vars
	var classification int8
	var ip net.IP
	var result profile.EPResult

	// filter function
	filterEntries := func(entries []dns.RR) (goodEntries []dns.RR) {
		goodEntries = make([]dns.RR, 0, len(entries))

		for _, rr := range entries {

			// get IP and classification
			switch v := rr.(type) {
			case *dns.A:
				ip = v.A
			case *dns.AAAA:
				ip = v.AAAA
			default:
				// add non A/AAAA entries
				goodEntries = append(goodEntries, rr)
				continue
			}
			classification = netutils.ClassifyIP(ip)

			if filterByScope {
				switch {
				case classification == netutils.HostLocal:
					// No DNS should return localhost addresses
					addressesRemoved++
					rrCache.FilteredEntries = append(rrCache.FilteredEntries, rr.String())
					continue
				case rrCache.ServerScope == netutils.Global && (classification == netutils.SiteLocal || classification == netutils.LinkLocal):
					// No global DNS should return LAN addresses
					addressesRemoved++
					rrCache.FilteredEntries = append(rrCache.FilteredEntries, rr.String())
					continue
				}
			}

			if filterByProfile {
				// filter by flags
				switch {
				case !profileSet.CheckFlag(profile.Internet) && classification == netutils.Global:
					addressesRemoved++
					rrCache.FilteredEntries = append(rrCache.FilteredEntries, rr.String())
					continue
				case !profileSet.CheckFlag(profile.LAN) && (classification == netutils.SiteLocal || classification == netutils.LinkLocal):
					addressesRemoved++
					rrCache.FilteredEntries = append(rrCache.FilteredEntries, rr.String())
					continue
				case !profileSet.CheckFlag(profile.Localhost) && classification == netutils.HostLocal:
					addressesRemoved++
					rrCache.FilteredEntries = append(rrCache.FilteredEntries, rr.String())
					continue
				}

				// filter by endpoints
				result, _ = profileSet.CheckEndpointIP(fqdn, ip, 0, 0, false)
				if result == profile.Denied {
					addressesRemoved++
					rrCache.FilteredEntries = append(rrCache.FilteredEntries, rr.String())
					continue
				}
			}

			// if survived, add to good entries
			addressesOk++
			goodEntries = append(goodEntries, rr)
		}
		return
	}

	rrCache.Answer = filterEntries(rrCache.Answer)
	rrCache.Extra = filterEntries(rrCache.Extra)

	if addressesRemoved > 0 {
		rrCache.Filtered = true
		if addressesOk == 0 {
			comm.Deny("no addresses returned for this domain are permitted")
			log.Infof("firewall: fully dns responses for communication %s", comm)
			return nil
		}
	}

	if rrCache.Filtered {
		log.Infof("firewall: filtered DNS replies for %s: %s", comm, strings.Join(rrCache.FilteredEntries, ", "))
	}

	// TODO: Gate17 integration
	// tunnelInfo, err := AssignTunnelIP(fqdn)

	return rrCache
}

// DecideOnCommunication makes a decision about a communication with its first packet.
func DecideOnCommunication(comm *network.Communication, pkt packet.Packet) {

	// check if communication needs reevaluation, if it's not with a domain
	if comm.NeedsReevaluation() {
		log.Infof("firewall: re-evaluating verdict on %s", comm)
		comm.ResetVerdict()

		// if communicating with a domain entity, re-evaluate with Before/AfterIntel
		if strings.HasSuffix(comm.Domain, ".") {
			DecideOnCommunicationBeforeIntel(comm, comm.Domain)
			DecideOnCommunicationAfterIntel(comm, comm.Domain, nil)
		}
	}

	// check if need to run
	if comm.GetVerdict() != network.VerdictUndecided {
		return
	}

	// grant self
	if comm.Process().Pid == os.Getpid() {
		log.Infof("firewall: granting own communication %s", comm)
		comm.Accept("")
		return
	}

	// check if there is a profile
	profileSet := comm.Process().ProfileSet()
	if profileSet == nil {
		log.Errorf("firewall: denying communication %s, no Profile Set", comm)
		comm.Deny("no Profile Set")
		return
	}
	profileSet.Update(status.ActiveSecurityLevel())

	// check comm type
	switch comm.Domain {
	case network.IncomingHost, network.IncomingLAN, network.IncomingInternet, network.IncomingInvalid:
		if !profileSet.CheckFlag(profile.Service) {
			log.Infof("firewall: denying communication %s, not a service", comm)
			if comm.Domain == network.IncomingHost {
				comm.Block("not a service")
			} else {
				comm.Deny("not a service")
			}
			return
		}
	case network.PeerLAN, network.PeerInternet, network.PeerInvalid: // Important: PeerHost is and should be missing!
		if !profileSet.CheckFlag(profile.PeerToPeer) {
			log.Infof("firewall: denying communication %s, peer to peer comms (to an IP) not allowed", comm)
			comm.Deny("peer to peer comms (to an IP) not allowed")
			return
		}
	}

	// check network scope
	switch comm.Domain {
	case network.IncomingHost:
		if !profileSet.CheckFlag(profile.Localhost) {
			log.Infof("firewall: denying communication %s, serving localhost not allowed", comm)
			comm.Block("serving localhost not allowed")
			return
		}
	case network.IncomingLAN:
		if !profileSet.CheckFlag(profile.LAN) {
			log.Infof("firewall: denying communication %s, serving LAN not allowed", comm)
			comm.Deny("serving LAN not allowed")
			return
		}
	case network.IncomingInternet:
		if !profileSet.CheckFlag(profile.Internet) {
			log.Infof("firewall: denying communication %s, serving Internet not allowed", comm)
			comm.Deny("serving Internet not allowed")
			return
		}
	case network.IncomingInvalid:
		log.Infof("firewall: denying communication %s, invalid IP address", comm)
		comm.Drop("invalid IP address")
		return
	case network.PeerHost:
		if !profileSet.CheckFlag(profile.Localhost) {
			log.Infof("firewall: denying communication %s, accessing localhost not allowed", comm)
			comm.Block("accessing localhost not allowed")
			return
		}
	case network.PeerLAN:
		if !profileSet.CheckFlag(profile.LAN) {
			log.Infof("firewall: denying communication %s, accessing the LAN not allowed", comm)
			comm.Deny("accessing the LAN not allowed")
			return
		}
	case network.PeerInternet:
		if !profileSet.CheckFlag(profile.Internet) {
			log.Infof("firewall: denying communication %s, accessing the Internet not allowed", comm)
			comm.Deny("accessing the Internet not allowed")
			return
		}
	case network.PeerInvalid:
		log.Infof("firewall: denying communication %s, invalid IP address", comm)
		comm.Deny("invalid IP address")
		return
	}

	log.Infof("firewall: undeterminable verdict for communication %s", comm)
	comm.UpdateVerdict(network.VerdictUndeterminable)
}

// DecideOnLink makes a decision about a link with the first packet.
func DecideOnLink(comm *network.Communication, link *network.Link, pkt packet.Packet) {

	// grant self
	if comm.Process().Pid == os.Getpid() {
		log.Infof("firewall: granting own link %s", comm)
		link.Accept("")
		return
	}

	// check if communicating with self
	if comm.Process().Pid >= 0 && pkt.Info().Src.Equal(pkt.Info().Dst) {
		// get PID
		otherPid, _, err := process.GetPidByEndpoints(
			pkt.Info().RemoteIP(),
			pkt.Info().RemotePort(),
			pkt.Info().LocalIP(),
			pkt.Info().LocalPort(),
			pkt.Info().Protocol,
		)
		if err == nil {

			// get primary process
			otherProcess, err := process.GetOrFindPrimaryProcess(pkt.Ctx(), otherPid)
			if err == nil {

				if otherProcess.Pid == comm.Process().Pid {
					log.Infof("firewall: permitting connection to self %s", comm)
					link.AddReason("connection to self")

					link.Lock()
					link.Verdict = network.VerdictAccept
					link.SaveWhenFinished()
					link.Unlock()
					return
				}

			}
		}
	}

	// check if we aleady have a verdict
	switch comm.GetVerdict() {
	case network.VerdictUndecided, network.VerdictUndeterminable:
		// continue
	default:
		link.UpdateVerdict(comm.GetVerdict())
		return
	}

	// check if there is a profile
	profileSet := comm.Process().ProfileSet()
	if profileSet == nil {
		log.Infof("firewall: no Profile Set, denying %s", link)
		link.Deny("no Profile Set")
		return
	}
	profileSet.Update(status.ActiveSecurityLevel())

	// get domain
	var fqdn string
	if strings.HasSuffix(comm.Domain, ".") {
		fqdn = comm.Domain
	}

	// remoteIP
	var remoteIP net.IP
	if comm.Direction {
		remoteIP = pkt.Info().Src
	} else {
		remoteIP = pkt.Info().Dst
	}

	// protocol and destination port
	protocol := uint8(pkt.Info().Protocol)
	dstPort := pkt.Info().DstPort

	// check endpoints list
	result, reason := profileSet.CheckEndpointIP(fqdn, remoteIP, protocol, dstPort, comm.Direction)
	switch result {
	case profile.Denied:
		log.Infof("firewall: denying link %s, endpoint is blacklisted: %s", link, reason)
		link.Deny(fmt.Sprintf("endpoint is blacklisted: %s", reason))
		return
	case profile.Permitted:
		log.Infof("firewall: permitting link %s, endpoint is whitelisted: %s", link, reason)
		link.Accept(fmt.Sprintf("endpoint is whitelisted: %s", reason))
		return
	}

	// TODO: Stamp integration

	switch profileSet.GetProfileMode() {
	case profile.Whitelist:
		log.Infof("firewall: denying link %s: endpoint is not whitelisted", link)
		link.Deny("endpoint is not whitelisted")
		return
	case profile.Blacklist:
		log.Infof("firewall: permitting link %s: endpoint is not blacklisted", link)
		link.Accept("endpoint is not blacklisted")
		return
	}

	// ProfileMode == Prompt

	// check relation
	if fqdn != "" && profileSet.CheckFlag(profile.Related) {
		if checkRelation(comm, fqdn) {
			return
		}
	}

	// first check if there is an existing notification for this.
	var nID string
	switch {
	case comm.Direction:
		nID = fmt.Sprintf("firewall-prompt-%d-%s-%s-%d-%d", comm.Process().Pid, comm.Domain, remoteIP, protocol, dstPort)
	case fqdn == "":
		nID = fmt.Sprintf("firewall-prompt-%d-%s-%s-%d-%d", comm.Process().Pid, comm.Domain, remoteIP, protocol, dstPort)
	default:
		nID = fmt.Sprintf("firewall-prompt-%d-%s-%s-%d-%d", comm.Process().Pid, comm.Domain, remoteIP, protocol, dstPort)
	}
	nTTL := 15 * time.Second
	n := notifications.Get(nID)

	if n != nil {
		// we were not here first, only get verdict, do not make changes
		select {
		case promptResponse := <-n.Response():
			switch promptResponse {
			case "permit-domain-all", "permit-domain-distinct", "permit-ip", "permit-ip-incoming":
				link.Accept("permitted by user")
			default:
				link.Deny("denied by user")
			}
		case <-time.After(nTTL):
			link.Deny("user did not respond to prompt")
		}
		return
	}

	// create new notification
	n = (&notifications.Notification{
		ID:      nID,
		Type:    notifications.Prompt,
		Expires: time.Now().Add(nTTL).Unix(),
	})

	switch {
	case comm.Direction:
		n.Message = fmt.Sprintf("Application %s wants to accept connections from %s (%d/%d)", comm.Process(), remoteIP, protocol, dstPort)
		n.AvailableActions = []*notifications.Action{
			&notifications.Action{
				ID:   "permit-ip-incoming",
				Text: fmt.Sprintf("Permit serving to %s", remoteIP),
			},
		}
	case fqdn == "":
		n.Message = fmt.Sprintf("Application %s wants to connect to %s (%d/%d)", comm.Process(), remoteIP, protocol, dstPort)
		n.AvailableActions = []*notifications.Action{
			&notifications.Action{
				ID:   "permit-ip",
				Text: fmt.Sprintf("Permit %s", remoteIP),
			},
		}
	default:
		n.Message = fmt.Sprintf("Application %s wants to connect to %s (%s %d/%d)", comm.Process(), comm.Domain, remoteIP, protocol, dstPort)
		n.AvailableActions = []*notifications.Action{
			&notifications.Action{
				ID:   "permit-domain-all",
				Text: fmt.Sprintf("Permit all %s", comm.Domain),
			},
			&notifications.Action{
				ID:   "permit-domain-distinct",
				Text: fmt.Sprintf("Permit %s", comm.Domain),
			},
		}
	}

	n.AvailableActions = append(n.AvailableActions, &notifications.Action{
		ID:   "deny",
		Text: "deny",
	})
	n.Init().Save()

	// react
	select {
	case promptResponse := <-n.Response():
		n.Cancel()

		new := &profile.EndpointPermission{
			Type:    profile.EptDomain,
			Value:   comm.Domain,
			Permit:  true,
			Created: time.Now().Unix(),
		}

		switch promptResponse {
		case "permit-domain-all":
			new.Value = "." + new.Value
		case "permit-domain-distinct":
			// everything already set
		case "permit-ip", "permit-ip-incoming":
			if pkt.Info().Version == packet.IPv4 {
				new.Type = profile.EptIPv4
			} else {
				new.Type = profile.EptIPv6
			}
			new.Value = remoteIP.String()
		default:
			// deny
			new.Permit = false
		}

		if new.Permit {
			log.Infof("firewall: user permitted link %s -> %s", comm.Process(), new.Value)
			link.Accept("permitted by user")
		} else {
			log.Infof("firewall: user denied link %s -> %s", comm.Process(), new.Value)
			link.Deny("denied by user")
		}

		profileSet.Lock()
		defer profileSet.Unlock()
		userProfile := profileSet.UserProfile()
		userProfile.Lock()
		defer userProfile.Unlock()

		if promptResponse == "permit-ip-incoming" {
			userProfile.ServiceEndpoints = append(userProfile.ServiceEndpoints, new)
		} else {
			userProfile.Endpoints = append(userProfile.Endpoints, new)
		}
		go userProfile.Save("")

	case <-time.After(nTTL):
		n.Cancel()
		link.Deny("user did not respond to prompt")

	}
}

func checkRelation(comm *network.Communication, fqdn string) (related bool) {
	profileSet := comm.Process().ProfileSet()
	if profileSet == nil {
		return
	}

	// TODO: add #AI

	pathElements := strings.Split(comm.Process().Path, "/") // FIXME: path seperator
	// only look at the last two path segments
	if len(pathElements) > 2 {
		pathElements = pathElements[len(pathElements)-2:]
	}
	domainElements := strings.Split(fqdn, ".")

	var domainElement string
	var processElement string

matchLoop:
	for _, domainElement = range domainElements {
		for _, pathElement := range pathElements {
			if levenshtein.Match(domainElement, pathElement, nil) > 0.5 {
				related = true
				processElement = pathElement
				break matchLoop
			}
		}
		if levenshtein.Match(domainElement, profileSet.UserProfile().Name, nil) > 0.5 {
			related = true
			processElement = profileSet.UserProfile().Name
			break matchLoop
		}
		if levenshtein.Match(domainElement, comm.Process().Name, nil) > 0.5 {
			related = true
			processElement = comm.Process().Name
			break matchLoop
		}
		if levenshtein.Match(domainElement, comm.Process().ExecName, nil) > 0.5 {
			related = true
			processElement = comm.Process().ExecName
			break matchLoop
		}
	}

	if related {
		log.Infof("firewall: permitting communication %s, match to domain was found: %s is related to %s", comm, domainElement, processElement)
		comm.Accept(fmt.Sprintf("domain is related to process: %s is related to %s", domainElement, processElement))
	}
	return
}
