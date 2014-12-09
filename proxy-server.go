package main

import "bytes"
import "flag"
import "fmt"
import "gss/proxy"
import "gss/misc"
import "net"
import "io"
import "os"
import "strconv"

func dump(file io.Writer, data []byte) {
	var another bool

	for i, b := range data {
		fmt.Fprintf(file, "%02x", b)
		if i%16 == 15 {
			fmt.Fprintf(file, "\n")
			another = false
		} else {
			fmt.Fprintf(file, " ")
			another = true
		}
	}
	if another {
		fmt.Fprintf(file, "\n")
	}
}

func serve(pconn *net.Conn, pcc proxy.CallCtx, conn net.Conn, cred *proxy.Cred, export, verbose bool, logfile io.Writer) {
	var pctx *proxy.SecCtx
	var cname proxy.Name

	defer conn.Close()

	tag, token := misc.RecvToken(conn)
	if tag == 0 && len(token) == 0 {
		fmt.Printf("EOF from client\n", tag)
		return
	}
	if (tag & misc.TOKEN_NOOP) == 0 {
		if logfile != nil {
			fmt.Fprintf(logfile, "Expected NOOP token, got %d token instead.\n", tag)
		}
		return
	}
	if (tag & misc.TOKEN_CONTEXT_NEXT) != 0 {
		/* Optionally export/reimport the acceptor cred a few times. */
		if export && cred != nil {
			for i := 0; i < 3; i++ {
				ecr, err := proxy.ExportCred(pconn, pcc, *cred, 0, nil)
				if err != nil {
					fmt.Fprintf(logfile, "Error exporting credential: %s\n", err)
					return
				}
				if ecr.Status.MajorStatus != proxy.S_COMPLETE {
					misc.DisplayProxyStatus("exporting a credential", ecr.Status)
					return
				}
				pcc.ServerCtx = ecr.Status.ServerCtx
				icr, err := proxy.ImportCred(pconn, pcc, ecr.ExportedHandle, nil)
				if err != nil {
					fmt.Fprintf(logfile, "Error importing credential: %s\n", err)
					return
				}
				if icr.Status.MajorStatus != proxy.S_COMPLETE {
					misc.DisplayProxyStatus("importing a credential", icr.Status)
					return
				}
				pcc.ServerCtx = icr.Status.ServerCtx
				cred = icr.OutputCredHandle
			}
		}
		for {
			/* Expect a context establishment token. */
			tag, token := misc.RecvToken(conn)
			if tag == 0 && len(token) == 0 {
				break
			}
			if verbose && logfile != nil {
				fmt.Fprintf(logfile, "Received token (%d bytes):\n", len(token))
				dump(logfile, token)
			}
			if tag&misc.TOKEN_CONTEXT == 0 {
				fmt.Printf("Expected context establishment token, got %d token instead.\n", tag)
				return
			}
			ascr, err := proxy.AcceptSecContext(pconn, pcc, pctx, cred, token, true, nil)
			if err != nil {
				fmt.Printf("Error accepting context: %s.\n", err)
				return
			}
			if ascr.Status.MajorStatus != proxy.S_COMPLETE && ascr.Status.MajorStatus != proxy.S_CONTINUE_NEEDED {
				misc.DisplayProxyStatus("accepting a context", ascr.Status)
				return
			}
			pcc.ServerCtx = ascr.Status.ServerCtx
			if ascr.SecCtx != nil {
				pctx = ascr.SecCtx
			}
			if ascr.OutputToken != nil {
				/* If we got a new token, send it to the client. */
				if verbose && logfile != nil {
					fmt.Fprintf(logfile, "Sending accept_sec_context token (%d bytes):\n", len(*ascr.OutputToken))
					dump(logfile, *ascr.OutputToken)
				}
				misc.SendToken(conn, misc.TOKEN_CONTEXT, *ascr.OutputToken)
			}
			/* We never use delegated creds, so if we got some, just make sure they get cleaned up. */
			if ascr.DelegatedCredHandle != nil {
				rcr, err := proxy.ReleaseCred(pconn, pcc, *ascr.DelegatedCredHandle)
				if err != nil {
					fmt.Printf("Error releasing delegated creds: %s.\n", err)
					return
				}
				if rcr.Status.MajorStatus != proxy.S_COMPLETE {
					misc.DisplayProxyStatus("releasing delegated credentials", rcr.Status)
					return
				}
			}
			if ascr.Status.MajorStatus == proxy.S_COMPLETE {
				/* Okay, success. */
				if verbose && logfile != nil {
					fmt.Fprintf(logfile, "\n")
				}
				break
			}
			/* Wait for another context establishment token. */
			if verbose && logfile != nil {
				fmt.Fprintf(logfile, "continue needed...\n")
			}
		}
		/* Make sure the context is cleaned up eventually. */
		defer proxy.ReleaseSecCtx(pconn, pcc, *pctx)
		/* Dig up information about the connection. */
		if verbose && logfile != nil {
			fmt.Fprintf(logfile, "Accepted connection using mechanism OID %s.\n", pctx.Mech)
		}
		/* Figure out the client's attributes and displayable and local names. */
		if verbose && logfile != nil {
			for _, attr := range pctx.SrcName.NameAttributes {
				fmt.Fprintf(logfile, "Attribute %s \"%s\"", attr.Attr, attr.Value)
				fmt.Fprintf(logfile, "\n")
				dump(logfile, attr.Value)
			}
		}
	} else {
		if logfile != nil {
			fmt.Fprintf(logfile, "Accepted unauthenticated connection.\n")
		}
	}
	/* Start processing message tokens from the client. */
	if pctx != nil {
		if len(cname.DisplayName) > 0 {
			fmt.Printf("Accepted connection: \"%s\"\n", cname.DisplayName)
		} else {
			fmt.Printf("Accepted connection.\n")
		}
	} else {
		fmt.Printf("Accepted unauthenticated connection.\n")
	}
	for {
		/* Read a request. */
		tag, token := misc.RecvToken(conn)
		if tag == 0 && len(token) == 0 {
			if verbose {
				fmt.Printf("EOF from client.\n")
			}
			return
		}
		/* Client indicates EOF with another NOOP token. */
		if tag&misc.TOKEN_NOOP != 0 {
			if logfile != nil {
				fmt.Fprintf(logfile, "NOOP token\n")
			}
			break
		}
		/* Expect data tokens. */
		if tag&misc.TOKEN_DATA == 0 {
			fmt.Printf("Expected data token, got %d token instead.\n", tag)
			break
		}
		if verbose && logfile != nil {
			fmt.Fprintf(logfile, "Message token (flags=%d):\n", tag)
			dump(logfile, token)
		}
		/* No context handle means no encryption or signing. */
		if pctx == nil && (tag&(misc.TOKEN_WRAPPED|misc.TOKEN_ENCRYPTED|misc.TOKEN_SEND_MIC)) != 0 {
			if logfile != nil {
				fmt.Fprintf(logfile, "Unauthenticated client requested authenticated services!\n")
			}
			break
		}
		/* If it's wrapped at all, unwrap it. */
		if tag&misc.TOKEN_WRAPPED != 0 {
			tokens := make([][]byte, 1)
			tokens[0] = token
			ur, err := proxy.Unwrap(pconn, pcc, *pctx, tokens, proxy.C_QOP_DEFAULT)
			if err != nil {
				fmt.Printf("Error unwrapping token: %s.\n", err)
				return
			}
			if ur.Status.MajorStatus != proxy.S_COMPLETE {
				misc.DisplayProxyStatus("unwrapping token", ur.Status)
				return
			}
			pcc.ServerCtx = ur.Status.ServerCtx
			if ur.SecCtx != nil {
				pctx = ur.SecCtx
			}
			/* If we were told it was encrypted, and it wasn't, warn. */
			if !ur.ConfState && misc.TOKEN_ENCRYPTED != 0 {
				fmt.Printf("Warning!  Message not encrypted.\n")
			}
			token = ur.TokenBuffer[0]
		}
		/* Log it. */
		if logfile != nil {
			fmt.Fprintf(logfile, "Received message: ")
			if token[0] >= 32 && token[0] < 127 && token[1] >= 32 && token[1] < 127 {
				buf := bytes.NewBuffer(token)
				fmt.Fprintf(logfile, "\"%s\"\n", buf)
			} else {
				fmt.Fprintf(logfile, "\n")
				dump(logfile, token)
			}
		}
		/* Reply. */
		if tag&misc.TOKEN_SEND_MIC != 0 {
			/* Send back a signature over the payload data. */
			gmr, err := proxy.GetMic(pconn, pcc, *pctx, proxy.C_QOP_DEFAULT, token)
			if err != nil {
				fmt.Printf("Error signing token: %s.\n", err)
				return
			}
			if gmr.Status.MajorStatus != proxy.S_COMPLETE {
				misc.DisplayProxyStatus("unwrapping token", gmr.Status)
				return
			}
			pcc.ServerCtx = gmr.Status.ServerCtx
			if gmr.SecCtx != nil {
				pctx = gmr.SecCtx
			}
			misc.SendToken(conn, misc.TOKEN_MIC, gmr.TokenBuffer)
		} else {
			/* Send back a minimal acknowledgement. */
			misc.SendToken(conn, misc.TOKEN_NOOP, nil)
		}
	}
}

func main() {
	port := flag.Int("port", 4444, "port")
	verbose := flag.Bool("verbose", false, "verbose")
	once := flag.Bool("once", false, "single-connection mode")
	export := flag.Bool("export", false, "export/reimport the context")
	logfile := flag.String("logfile", "/dev/stdout", "log file for details")
	var sname proxy.Name
	var call proxy.CallCtx
	var log *os.File
	var err error

	flag.Parse()
	if flag.NArg() < 2 {
		fmt.Printf("Usage: gss-server [options] socket gss-service-name\n")
		flag.PrintDefaults()
		os.Exit(1)
	}
	sockaddr := flag.Arg(0)
	service := flag.Arg(1)

	/* Open the log file. */
	if logfile != nil {
		log, err = os.OpenFile(*logfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Printf("Error opening log file \"%s\": %s\n", *logfile, err)
			return
		}
	}

	/* Connect to the proxy. */
	pconn, err := net.Dial("unix", sockaddr)
	if err != nil {
		fmt.Printf("Error connecting to gss-proxy at \"%s\": %s", sockaddr, err)
		return
	}

	/* Get a calling context. */
	ctr, err := proxy.GetCallContext(&pconn, call, nil)
	if err != nil {
		fmt.Printf("Error getting a calling context: %s", err)
		return
	}
	call.ServerCtx = ctr.ServerCtx

	/* Set up the listener socket. */
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(*port))
	if err != nil {
		fmt.Printf("Error listening for client connection: %s\n", err)
		return
	}
	defer listener.Close()

	/* Set up the server's name. */
	sname.DisplayName = service
	sname.NameType = proxy.NT_HOSTBASED_SERVICE
	if false {
		icnr, err := proxy.ImportAndCanonName(&pconn, call, sname, nil, nil, nil)
		if err != nil {
			fmt.Printf("Error importing name: %s\n", err)
			return
		}
		if icnr.Status.MajorStatus != proxy.S_COMPLETE {
			misc.DisplayProxyStatus("importing name", icnr.Status)
			return
		}
		sname = *icnr.Name
		call.ServerCtx = icnr.Status.ServerCtx
	}

	/* Make sure we have acceptor creds for the service name. */
	acr, err := proxy.AcquireCred(&pconn, call, nil, false, &sname, proxy.C_INDEFINITE, nil, proxy.C_ACCEPT, proxy.C_INDEFINITE, proxy.C_INDEFINITE, nil)
	if err != nil {
		fmt.Printf("Error acquiring credentials: %s\n", err)
		return
	}
	if acr.OutputCredHandle == nil {
		fmt.Printf("No credentials acquired.\n")
		return
	}
	if acr.Status.MajorStatus != proxy.S_COMPLETE {
		misc.DisplayProxyStatus("acquiring credentials", acr.Status)
		return
	}
	call.ServerCtx = acr.Status.ServerCtx
	cred := acr.OutputCredHandle

	fmt.Printf("starting...\n")
	if *once {
		/* Service exactly one client. */
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("Error accepting client connection: %s\n", err)
			return
		}
		serve(&pconn, call, conn, cred, *export, *verbose, log)
	} else {
		/* Just keep serving clients. */
		for {
			conn, err := listener.Accept()
			if err != nil {
				fmt.Printf("Error accepting client connection: %s\n", err)
				continue
			}
			go serve(&pconn, call, conn, cred, *export, *verbose, log)
		}
	}
	return
}
