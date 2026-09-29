# Generates internal/importer/testdata/mobaxterm_full.mxtsessions from the documented field tables
# (gist Ruzgfpegk/ab597838e4abbe8de30d7224afd062ea, MobaXterm 26.3). CP1252, CRLF.
# Usage: python3 gen_mobaxterm_full.py "<[SSH_Hostkeys] line>"... > mobaxterm_full.mxtsessions
# (the three host key lines are the ones in the committed fixture; their OpenSSH form is in
# mobaxterm_full.hostkeys.expected).
import sys

DEFAULT_TERM = "MobaFont%10%0%0%-1%15%236,236,236%30,30,30%180,180,192%0%-1%0%%xterm%-1%0%_Std_Colors_0_%80%24%0%1%-1%<none>%%0%0%-1%0%"

def fields(n, **kv):
    f = [""] * n
    for k, v in kv.items():
        f[int(k[1:])] = v
    return "%".join(f)

def ssh(**kv):
    base = dict(f0="0", f2="22", f5="-1", f6="-1", f11="0", f12="0", f13="0", f16="-1", f17="0", f18="0", f19="0",
                f21="1080", f23="0", f24="0", f25="1", f27="0", f31="0", f32="-1", f33="-1", f34="0", f37="0")
    base.update(kv)
    return fields(39, **base)

def line(name, icon, block, term=DEFAULT_TERM, start="0", comment=" ", color="-1", logout=" "):
    return f"{name}={logout}#{icon}#{block}#{term}#{start}#{comment}#{color}"

colors16 = "%".join(["0,0,0", "54,54,54", "255,96,96", "255,128,128", "96,255,96", "128,255,128", "255,255,54",
                     "255,255,128", "96,96,255", "128,128,255", "255,54,255", "255,128,255", "54,255,255",
                     "128,255,255", "236,236,236", "255,255,255"])
custom_term_fields = ["MobaFont", "12", "0", "0", "-1", "13", "236,236,236", "0,0,0", "180,180,192", "4", "-1", "-1", "",
                      "vt220", "-1", "0"] + colors16.split("%") + ["80", "24", "0", "1", "-1", "<custom macro>",
                      "12:2:0:uptime__PIPE__", "0", "1", "-1", "0", "1001111111110000000000,x2,1,,,,,R,S,Any,0,5000"]
custom_term = "%".join(custom_term_fields)

rdp = fields(32, f0="4", f1="win01.corp.example", f2="3390", f3="CORP\\alice", f4="-1", f5="0", f6="-1", f7="0", f8="-1",
             f9="0", f10="11", f11="-1", f12="C:\\Tools\\start.cmd", f13="gw.example.com", f14="2200", f15="jump",
             f16="1", f17="0", f18="_CurrentDrive_:\\keys\\gw.ppk", f19="0", f20="", f21="-1", f22="-1", f23="0",
             f24="-1", f25="-1", f26="-1", f27="0", f28="4", f29="0", f30="0",
             f31="100100111000,2,rdgw.corp.example,CORP\\gwuser,0,2")
rdp2 = fields(32, f0="4", f1="win02.corp.example", f2="3389", f3="bob", f13="gw.example.com", f14="2200",
              f15="jump", f16="0", f19="-1", f28="0", f31="100000100000,0,,,0,0")
vnc = fields(18, f0="5", f1="vnc.example.com", f2="5901", f3="0", f4="-1", f5="hop1.example.com__PIPE__hop2.example.com",
             f6="22__PIPE__2022", f7="u1__PIPE__u2", f8="", f9="-1", f10="0", f11="-1", f12="", f13="2",
             f14="socks.example.com", f15="1081", f16="proxyuser")
sftp = fields(17, f0="7", f1="files.example.com", f2="2022", f3="backup", f4="-1", f5="-1", f6="/srv/backup", f7="0",
              f8="0", f9="_CurrentDrive_:\\keys\\backup.ppk", f10="4", f11="proxy.example.com", f12="1085",
              f13="puser", f14="Sup3rS3cret!", f15="C:\\local", f16="-1")

lines = []
lines.append("[Bookmarks]")
lines.append("SubRep=")
lines.append("ImgNum=42")
# Reference-style default SSH session (leading space), default icon.
lines.append(line("Reference Session", "109", ssh(f1="localhost", f3="<default>")))
# Custom tab colour (pure red = 255), custom icon 196 (Docker), comment with an escaped '#', logout prefix.
lines.append(line("docker-host", "196", ssh(f1="docker.example.com", f3="root", f7="docker ps", f33="0"),
                  comment="Build box __DIEZE__1", color="255", logout=";  logout"))
lines.append("")
lines.append("[Bookmarks_1]")
lines.append("SubRep=Production")
lines.append("ImgNum=41")
lines.append(line("web1", "109", ssh(f1="web1.example.com", f2="2222", f3="deploy", f7="tmux new -A -s main",
                                      f11="-1", f14="_CurrentDrive_:\\Users\\me\\keys\\web.ppk", f25="2", f34="-1"),
                  term=custom_term, comment="Primary web server", color="16711680"))
lines.append(line("db1", "109", ssh(f1="10.0.0.9", f3="dbadmin", f5="0", f6="0",
                                     f8="gw1.example.com__PIPE__gw2.example.com", f9="22__PIPE__2222",
                                     f10="u1__PIPE__jumpuser", f15="__PIPE___CurrentDrive_:\\keys\\gw2.ppk",
                                     f19="2", f20="socks.example.com", f21="1080", f22="sockuser")))
lines.append(line("local-cmd-proxy", "109", ssh(f1="internal.example.com", f3="ops", f19="5",
                                                 f26="nc -X connect -x proxy:3128 __PERCENT__host __PERCENT__port")))
lines.append(line("ssh-proxy", "109", ssh(f1="behind.example.com", f3="ops", f19="6", f20="bastion.example.com",
                                           f21="2022", f22="hopper")))
lines.append("")
lines.append("[Bookmarks_2]")
lines.append("SubRep=Production\\Network\\Core\\Edge")
lines.append("ImgNum=41")
lines.append(line("switch1", "116", fields(20, f0="1", f1="192.168.1.2", f2="2323", f3="operator")))
lines.append(line("rsh-box", "100", fields(4, f0="2", f1="legacy.example.com", f2="oper", f3="")))
lines.append(line("ftp-site", "130", fields(20, f0="6", f1="ftp.example.com", f2="2121", f3="ftpuser")))
lines.append("")
lines.append("[Bookmarks_3]")
lines.append("SubRep=Windows")
lines.append("ImgNum=41")
lines.append(line("win01", "91", rdp))
lines.append(line("win02", "91", rdp2))
lines.append(line("console", "128", vnc))
lines.append(line("backup", "140", sftp))
lines.append(line("console-serial", "131", "8%0%1009600%3%0%0%1%2%COM2  (USB Serial (COM2))"))
lines.append(line("roamer", "145", fields(20, f0="12", f1="mosh.example.com", f2="2222", f3="mobile",
                                           f6="_CurrentDrive_:\\keys\\mosh.ppk")))
lines.append(line("Caf\u00e9 \u00e9t\u00e9", "109", ssh(f1="cafe.example.com", f3="\u00e9lise")))
lines.append(line("empty-host", "109", ssh(f1="")))
lines.append(line("localsh", "97", "10%0%"))
lines.append(line("files", "84", "9%C:\\Users\\me\\Desktop%0%"))
lines.append(line("xdmcp", "88", "3%%0%-1%0%0%0%localhost%7100%1%0%1%0%657%336%0%0"))
lines.append(line("portal", "313", "11%https://portal.example.com%-1%-1%-1%-1%-1%-1%-1%0%0%3"))
lines.append(line("bucket", "343", "13%s3.amazonaws.com%%AKIAEXAMPLE%%"))
lines.append(line("ubuntu-wsl", "151", "14%Ubuntu%"))
lines.append("")
lines.append("[SSH_Hostkeys]")
for l in sys.argv[1:]:
    lines.append(l)
lines.append("")
data = "\r\n".join(lines) + "\r\n"
sys.stdout.buffer.write(data.encode("cp1252"))
