package dns

import (
	"crypto/sha1"
	"hash"
	"io"
	"strings"
)

const (
	_ = iota
	NSEC3_NXDOMAIN
	NSEC3_NODATA
)

type saltWireFmt struct {
	Salt string `dns:"size-hex"`
}

// HashName hashes a string (label) according to RFC5155. It returns the hashed string.
func HashName(label string, ha uint8, iter uint16, salt string) string {
	saltwire := new(saltWireFmt)
	saltwire.Salt = salt
	wire := make([]byte, DefaultMsgSize)
	n, ok := packStruct(saltwire, wire, 0)
	if !ok {
		return ""
	}
	wire = wire[:n]
	name := make([]byte, 255)
	off, ok1 := PackDomainName(strings.ToLower(label), name, 0, nil, false)
	if !ok1 {
		return ""
	}
	name = name[:off]
	var s hash.Hash
	switch ha {
	case SHA1:
		s = sha1.New()
	default:
		return ""
	}

	// k = 0
	name = append(name, wire...)
	io.WriteString(s, string(name))
	nsec3 := s.Sum(nil)
	// k > 0
	for k := uint16(0); k < iter; k++ {
		s.Reset()
		nsec3 = append(nsec3, wire...)
		io.WriteString(s, string(nsec3))
		nsec3 = s.Sum(nil)
	}
	return unpackBase32(nsec3)
}

// HashNames hashes the ownername and the next owner name in an NSEC3 record according to RFC 5155.
// It uses the paramaters as set in the NSEC3 record. The string zone is appended to the hashed
// ownername.
func (nsec3 *RR_NSEC3) HashNames(zone string) {
	nsec3.Header().Name = strings.ToLower(HashName(nsec3.Header().Name, nsec3.Hash, nsec3.Iterations, nsec3.Salt)) + "." + zone
	nsec3.NextDomain = HashName(nsec3.NextDomain, nsec3.Hash, nsec3.Iterations, nsec3.Salt)
}

// Match checks if domain matches the first (hashed) owner name of the NSEC3 record. Domain must be given
// in plain text.
func (nsec3 *RR_NSEC3) Match(domain string) bool {
	return strings.ToUpper(SplitLabels(nsec3.Header().Name)[0]) == strings.ToUpper(HashName(domain, nsec3.Hash, nsec3.Iterations, nsec3.Salt))
}

// Cover checks if domain is covered by the NSEC3 record. Domain must be given in plain text.
func (nsec3 *RR_NSEC3) Cover(domain string) bool {
	hashdom := strings.ToUpper(HashName(domain, nsec3.Hash, nsec3.Iterations, nsec3.Salt))
	nextdom := strings.ToUpper(nsec3.NextDomain)
	owner := strings.ToUpper(SplitLabels(nsec3.Header().Name)[0])                                                                              // The hashed part
	apex := strings.ToUpper(HashName(strings.Join(SplitLabels(nsec3.Header().Name)[1:], "."), nsec3.Hash, nsec3.Iterations, nsec3.Salt)) + "." // The name of the zone
	// if nextdomain equals the apex, it is considered The End. So in that case hashdom is always less then nextdomain
	if hashdom > owner && nextdom == apex {
		return true
	}

	if hashdom > owner && hashdom <= nextdom {
		return true
	}

	return false
}

// Cover checks if domain is covered by the NSEC record. Domain must be given in plain text.
func (nsec *RR_NSEC) Cover(domain string) bool {
	return false
}

// NsecVerify verifies an denial of existence response with NSECs
// NsecVerify returns nil when the NSECs in the message contain
// the correct proof. This function does not validates the NSECs.
func (m *Msg) NsecVerify(q Question) error {

	return nil
}

// Nsec3Verify verifies an denial of existence response with NSEC3s.
// This function does not validate the NSEC3s.
func (m *Msg) Nsec3Verify(q Question) (int, error) {
	var (
		nsec3    []*RR_NSEC3
		ncdenied = false // next closer denied
		sodenied = false // source of synthesis denied
		ce       = ""    // closest encloser
		nc       = ""    // next closer
		so       = ""    // source of synthesis
	)
	if len(m.Answer) > 0 && len(m.Ns) > 0 {
		// Wildcard expansion
		// Closest encloser inferred from SIG in authority and qname
		// println("EXPANDED WILDCARD PROOF or DNAME CNAME")
		// println("NODATA")
		// I need to check the type bitmap
		// wildcard bit not set?
		// MM: No need to check the wildcard bit here:
		//     This response has only 1 NSEC4 and it does not match
		//     the closest encloser (it covers next closer).
	}
	if len(m.Answer) == 0 && len(m.Ns) > 0 {
		// Maybe an NXDOMAIN or NODATA, we only know when we check
		for _, n := range m.Ns {
			if n.Header().Rrtype == TypeNSEC3 {
				nsec3 = append(nsec3, n.(*RR_NSEC3))
			}
		}
		if len(nsec3) == 0 {
			return 0, ErrDenialNsec3
		}

		lastchopped := ""
		labels := SplitLabels(q.Name)

		// Find the closest encloser and create the next closer
		for _, nsec := range nsec3 {
			candidate := ""
			for i := len(labels) - 1; i >= 0; i-- {
				candidate = labels[i] + "." + candidate
				if nsec.Match(candidate) {
					ce = candidate
				}
				lastchopped = labels[i]
			}
		}
		if ce == "" { // what about root label?
			return 0, ErrDenialCe
		}
		nc = lastchopped + "." + ce
		so = "*." + ce
		// Check if the next closer is covered and thus denied
		for _, nsec := range nsec3 {
			if nsec.Cover(nc) {
				ncdenied = true
				break
			}
		}
		if !ncdenied {
			if m.MsgHdr.Rcode == RcodeNameError {
				// For NXDOMAIN this is a problem
				return 0, ErrDenialNc // add next closer name here
			}
			goto NoData
		}

		// Check if the source of synthesis is covered and thus also denied
		for _, nsec := range nsec3 {
			if nsec.Cover(so) {
				sodenied = true
				break
			}
		}
		if !sodenied {
			return 0, ErrDenialSo
		}
		// The message headers claims something different!
		if m.MsgHdr.Rcode != RcodeNameError {
			return 0, ErrDenialHdr
		}

		return NSEC3_NXDOMAIN, nil
	}
	return 0, nil
NoData:
	// For NODATA we need to to check if the matching nsec3 has to correct type bit map
	// And we need to check that the wildcard does NOT exist
	for _, nsec := range nsec3 {
		if nsec.Cover(so) {
			sodenied = true
			break
		}
	}
	if sodenied {
		// Whoa, the closest encloser is denied, but there does exist
		// a wildcard a that level. That's not good
		return 0, ErrDenialWc
	}

	// The closest encloser MUST be the query name
	for _, nsec := range nsec3 {
		if nsec.Match(nc) {
			// This nsec3 must NOT have the type bitmap set of the qtype. If it does have it, return an error
			for _, t := range nsec.TypeBitMap {
				if t == q.Qtype {
					return 0, ErrDenialBit
				}
			}
		}
	}
	if m.MsgHdr.Rcode == RcodeNameError {
		return 0, ErrDenialHdr
	}
	return NSEC3_NODATA, nil
}
