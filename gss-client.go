package main

import "bytes"
import "flag"
import "fmt"
import "gss"
import "gss/misc"
import "net"
import "os"
import "strconv"
import "strings"
import "encoding/asn1"

func connectOnce(host string, port int, service string, mcount int, quiet bool, user, pass *string, plain []byte, v1, spnego bool, pmech *asn1.ObjectIdentifier, delegate, seq, noreplay, nomutual, noauth, nowrap, noenc, nomic bool) {
	const (
		TOKEN_NOOP    byte = (1 << 0)
		TOKEN_CONTEXT byte = (1 << 1)
		TOKEN_DATA    byte = (1 << 2)
		TOKEN_MIC     byte = (1 << 3)

		TOKEN_CONTEXT_NEXT byte = (1 << 4)
		TOKEN_WRAPPED      byte = (1 << 5)
		TOKEN_ENCRYPTED    byte = (1 << 6)
		TOKEN_SEND_MIC     byte = (1 << 7)
	)
	var ctx gss.ContextHandle
	var cred gss.CredHandle
	var mech asn1.ObjectIdentifier
	var tag byte
	var token []byte
	var major, minor uint32
	var sname, localstate, openstate string
	var flags gss.Flags

	/* Open the connection. */
	conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		fmt.Printf("Error connecting: %s\n", err)
		os.Exit(2)
	}
	defer conn.Close()

	/* Import the remote service's name. */
	if strings.Contains(service, "@") {
		sname = service
	} else {
		sname = service + "@" + host
	}
	major, minor, name := gss.ImportName(sname, gss.C_NT_HOSTBASED_SERVICE)
	if major != 0 {
		misc.DisplayError("importing remote service name", major, minor, nil)
		return
	}
	defer gss.ReleaseName(name)

	/* If we were passed a user name and/or password, acquire some initiator creds. */
	if user != nil {
		var mechSet []asn1.ObjectIdentifier

		/* Parse the user name. */
		major, minor, username := gss.ImportName(*user, gss.C_NT_USER_NAME)
		if major != 0 {
			misc.DisplayError("importing client name", major, minor, nil)
			return
		}
		defer gss.ReleaseName(username)

		/* Set the mechanism OID. */
		if pmech != nil || spnego {
			mechSet = make([]asn1.ObjectIdentifier, 1)
			if spnego {
				mechSet[0] = parseOid("1.3.6.1.5.5.2")
			} else {
				mechSet[0] = *pmech
			}
		} else {
			mechSet = nil
		}

		/* Acquire the creds. */
		if pass != nil {
			buffer := bytes.NewBufferString(*pass)
			password := buffer.Bytes()
			major, minor, cred, _, _ = gss.AcquireCredWithPassword(username, password, gss.C_INDEFINITE, mechSet, gss.C_INITIATE)
		} else {
			major, minor, cred, _, _ = gss.AcquireCred(username, gss.C_INDEFINITE, mechSet, gss.C_INITIATE)
		}
		if major != 0 {
			misc.DisplayError("acquiring creds", major, minor, &mechSet[0])
			return
		}
		defer gss.ReleaseCred(cred)
	}

	/* If we're doing SPNEGO, then a passed-in mechanism OID is the one we
	 * want to negotiate. */
	if spnego {
		if pmech != nil {
			mechSet := make([]asn1.ObjectIdentifier, 1)
			mechSet[0] = *pmech
			major, minor = gss.SetNegMechs(cred, mechSet)
			if major != gss.S_COMPLETE {
				misc.DisplayError("setting negotiate mechs", major, minor, nil)
				return
			}
		}
		mech = parseOid("1.3.6.1.5.5.2")
	} else {
		if pmech != nil {
			mech = *pmech
		} else {
			mech = nil
		}
	}

	if !v1 {
		misc.SendToken(conn, TOKEN_NOOP|TOKEN_CONTEXT_NEXT, nil)
	}

	if noauth {
		misc.SendToken(conn, TOKEN_NOOP, nil)
	} else {
		flags = gss.Flags{Deleg: delegate, Sequence: seq, Replay: !noreplay, Conf: !noenc, Integ: !nomic, Mutual: !nomutual}
		for true {
			/* Start/continue. */
			major, minor, _, token, flags, _, _, _ = gss.InitSecContext(cred, &ctx, name, mech, flags, 0, nil, token)
			if major != gss.S_COMPLETE && major != gss.S_CONTINUE_NEEDED {
				misc.DisplayError("initializing security context", major, minor, &mech)
				gss.DeleteSecContext(ctx)
				return
			}
			/* If we have an output token, we need to send it. */
			if len(token) > 0 {
				if !quiet {
					fmt.Printf("Sending init_sec_context token (%d bytes)...", len(token))
				}
				if v1 {
					tag = TOKEN_CONTEXT
				} else {
					tag = 0
				}
				misc.SendToken(conn, tag, token)
			}
			if major == gss.S_CONTINUE_NEEDED {
				/* CONTINUE_NEEDED means we expect a token from the far end to be fed back in to InitSecContext(). */
				if !quiet {
					fmt.Printf("continue needed...")
				}
				tag, token = misc.RecvToken(conn)
				if !quiet {
					fmt.Printf("\nReceived new input token (%d bytes).\n", len(token))
				}
			} else {
				/* COMPLETE means we're done, everything succeeded. */
				if !quiet {
					fmt.Printf("Done authenticating.\n")
				}
				defer gss.DeleteSecContext(ctx)
				break
			}
		}
		if major != gss.S_COMPLETE {
			fmt.Printf("Error authenticating to server: %x/%x.\n", major, minor)
			return
		}
		misc.DisplayFlags(flags)

		/* Describe the context. */
		major, minor, sname, tname, lifetime, mech, flags, _, _, local, open := gss.InquireContext(ctx)
		if major != gss.S_COMPLETE {
			misc.DisplayError("inquiring context", major, minor, &mech)
			return
		}
		major, minor, srcname, srcnametype := gss.DisplayName(sname)
		if major != gss.S_COMPLETE {
			misc.DisplayError("displaying source name", major, minor, &mech)
			return
		}
		major, minor, targname, _ := gss.DisplayName(tname)
		if major != gss.S_COMPLETE {
			misc.DisplayError("displaying target name", major, minor, &mech)
			return
		}
		if local {
			localstate = "locally initiated"
		} else {
			localstate = "remotely initiated"
		}
		if open {
			openstate = "open"
		} else {
			openstate = "closed"
		}
		fmt.Printf("\"%s\" to \"%s\", lifetime %d, %s, %s\n", srcname, targname, lifetime, localstate, openstate)
		misc.DisplayFlags(flags)
		fmt.Printf("Name type of source name is %s.\n", srcnametype)
		major, minor, mechs := gss.InquireNamesForMech(mech)
		if major != gss.S_COMPLETE {
			misc.DisplayError("inquiring mech names", major, minor, &mech)
			return
		}
		fmt.Printf("Mechanism %s supports %d names\n", mech, len(mechs))
		for i, nametype := range mechs {
			major, minor, oid := gss.OidToStr(nametype)
			if major != gss.S_COMPLETE {
				misc.DisplayError("converting OID to string", major, minor, &mech)
			} else {
				fmt.Printf("%3d: %s\n", i, oid)
			}
		}
	}

	for i := 0; i < mcount; i++ {
		var wrapped []byte
		var major, minor uint32
		var encrypted bool

		if nowrap {
			wrapped = plain
		} else {
			major, minor, encrypted, wrapped = gss.Wrap(ctx, !noenc, gss.C_QOP_DEFAULT, plain)
			if major != gss.S_COMPLETE {
				misc.DisplayError("wrapping data", major, minor, &mech)
				return
			}
		}
		if !noenc && !encrypted {
			fmt.Printf("Warning!  Message not encrypted.\n")
		}

		tag = TOKEN_DATA
		if !nowrap {
			tag |= TOKEN_WRAPPED
		}
		if !noenc {
			tag |= TOKEN_ENCRYPTED
		}
		if !nomic {
			tag |= TOKEN_SEND_MIC
		}
		if v1 {
			tag = 0
		}

		misc.SendToken(conn, tag, wrapped)
		_, mictoken := misc.RecvToken(conn)
		if nomic {
			if bytes.Equal(plain, mictoken) {
				fmt.Printf("Response differed.\n")
				return
			}
			if !quiet {
				fmt.Printf("Response received.\n")
			}
		} else {
			major, minor, _ = gss.VerifyMIC(ctx, plain, mictoken)
			if major != gss.S_COMPLETE {
				misc.DisplayError("verifying signature", major, minor, &mech)
				return
			}
			if !quiet {
				fmt.Printf("Signature verified.\n")
			}
		}
	}
	if !v1 {
		misc.SendToken(conn, TOKEN_NOOP, nil)
	}
}

func parseOid(oids string) (oid asn1.ObjectIdentifier) {
	components := strings.Split(oids, ".")
	if len(components) > 0 {
		oid = make([]int, len(components))
		for i, component := range components {
			val, err := strconv.Atoi(component)
			if err != nil {
				fmt.Printf("Error parsing OID \"%s\".\n", oids)
				oid = nil
				return
			}
			oid[i] = val
		}
	}
	return
}

func main() {
	port := flag.Int("port", 4444, "port")
	mechstr := flag.String("mech", "", "mechanism")
	spnego := flag.Bool("spnego", false, "use SPNEGO")
	iakerb := flag.Bool("iakerb", false, "use IAKERB")
	krb5 := flag.Bool("krb5", false, "use Kerberos 5")
	delegate := flag.Bool("d", false, "delegate")
	seq := flag.Bool("seq", false, "use sequence number checking")
	noreplay := flag.Bool("noreplay", false, "disable replay checking")
	nomutual := flag.Bool("nomutual", false, "perform one-way authentication")
	user := flag.String("user", "", "user name")
	pass := flag.String("pass", "", "password")
	file := flag.Bool("f", false, "read message from file")
	v1 := flag.Bool("v1", false, "use version 1 protocol")
	quiet := flag.Bool("q", false, "quiet")
	ccount := flag.Int("ccount", 1, "connection count")
	mcount := flag.Int("mcount", 1, "message count")
	noauth := flag.Bool("na", false, "no authentication")
	nowrap := flag.Bool("nw", false, "no wrapping")
	noenc := flag.Bool("nx", false, "no encryption")
	nomic := flag.Bool("nm", false, "no MICs")
	var plain []byte
	var mech *asn1.ObjectIdentifier

	flag.Parse()
	host := flag.Arg(0)
	service := flag.Arg(1)
	msg := flag.Arg(2)
	if flag.NArg() < 3 {
		flag.Usage()
		os.Exit(1)
	}

	if *file {
		msgfile, err := os.Open(msg)
		if err != nil {
			fmt.Printf("Error opening \"%s\": %s", msg, err)
			return
		}
		fi, err := msgfile.Stat()
		if err != nil {
			fmt.Printf("Error statting \"%s\": %s", msg, err)
			return
		}
		plain = make([]byte, fi.Size())
		n, err := msgfile.Read(plain)
		if int64(n) != fi.Size() {
			fmt.Printf("Error reading \"%s\": %s", msg, err)
			return
		}
	} else {
		buffer := bytes.NewBufferString(msg)
		plain = buffer.Bytes()
	}
	if *krb5 {
		tmpmech := parseOid("1.3.5.1.5.2")
		mech = &tmpmech
	}
	if *iakerb {
		tmpmech := parseOid("1.3.6.1.5.2.5")
		mech = &tmpmech
	}
	if len(*mechstr) > 0 {
		tmpmech := parseOid(*mechstr)
		mech = &tmpmech
	}
	if len(*user) == 0 {
		user = nil
	}
	if len(*pass) == 0 {
		pass = nil
	}

	for c := 0; c < *ccount; c++ {
		connectOnce(host, *port, service, *mcount, *quiet, user, pass, plain, *v1, *spnego, mech, *delegate, *seq, *noreplay, *nomutual, *noauth, *nowrap, *noenc, *nomic)
	}
}