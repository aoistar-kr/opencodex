// EnterForceSubmit - Windows-only Enter bridge for the Codex desktop composer.
//
// When the Codex desktop app hard-blocks sending because of a usage limit - the composer
// is still editable, text is present, the native send button is disabled and the
// rate-limit banner is on screen - a bare Enter still reaches the app and changes nothing,
// so this helper observes that outcome and submits the untouched draft through the
// OpenCodex force-submit bridge. No key is ever swallowed: the hook only records the press
// and an off-hook worker decides from before/after UI Automation snapshots.
//
// It touches nothing else: no app.asar, no Codex app config, no app-server bridge files.
//
// Build: build.ps1 (csc from the .NET Framework 4.0 directory). Run: run.ps1.

using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.Globalization;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;
using System.Windows.Automation;

internal static class Native
{
    public const int WH_KEYBOARD_LL = 13;
    public const int WM_KEYDOWN = 0x0100;
    public const int WM_KEYUP = 0x0101;
    public const int WM_SYSKEYDOWN = 0x0104;
    public const int WM_SYSKEYUP = 0x0105;
    public const int WM_QUIT = 0x0012;
    public const int VK_RETURN = 0x0D;
    public const int VK_SHIFT = 0x10;
    public const int VK_CONTROL = 0x11;
    public const int VK_MENU = 0x12;
    public const int VK_LWIN = 0x5B;
    public const int VK_RWIN = 0x5C;
    public const uint LLKHF_ALTDOWN = 0x20;
    public const uint COINIT_MULTITHREADED = 0x0;

    [StructLayout(LayoutKind.Sequential)]
    public struct KBDLLHOOKSTRUCT
    {
        public uint vkCode;
        public uint scanCode;
        public uint flags;
        public uint time;
        public IntPtr dwExtraInfo;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct MSG
    {
        public IntPtr hwnd;
        public uint message;
        public IntPtr wParam;
        public IntPtr lParam;
        public uint time;
        public int ptX;
        public int ptY;
    }

    public delegate IntPtr HookProcDelegate(int nCode, IntPtr wParam, IntPtr lParam);
    public delegate bool EnumWindowsProc(IntPtr hWnd, IntPtr lParam);

    [DllImport("user32.dll", SetLastError = true)]
    public static extern IntPtr SetWindowsHookExW(int idHook, HookProcDelegate lpfn, IntPtr hMod, uint dwThreadId);

    [DllImport("user32.dll")]
    public static extern bool UnhookWindowsHookEx(IntPtr hhk);

    [DllImport("user32.dll")]
    public static extern IntPtr CallNextHookEx(IntPtr hhk, int nCode, IntPtr wParam, IntPtr lParam);

    [DllImport("user32.dll")]
    public static extern int GetMessageW(out MSG lpMsg, IntPtr hWnd, uint wMsgFilterMin, uint wMsgFilterMax);

    [DllImport("user32.dll")]
    public static extern bool TranslateMessage(ref MSG lpMsg);

    [DllImport("user32.dll")]
    public static extern IntPtr DispatchMessageW(ref MSG lpMsg);

    [DllImport("user32.dll")]
    public static extern bool PostThreadMessageW(uint idThread, uint msg, IntPtr wParam, IntPtr lParam);

    [DllImport("user32.dll")]
    public static extern IntPtr GetForegroundWindow();

    [DllImport("user32.dll")]
    public static extern uint GetWindowThreadProcessId(IntPtr hWnd, out uint lpdwProcessId);

    [DllImport("kernel32.dll")]
    public static extern uint GetCurrentThreadId();

    [DllImport("user32.dll")]
    public static extern short GetAsyncKeyState(int vKey);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetClassNameW(IntPtr hWnd, StringBuilder lpClassName, int nMaxCount);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetWindowTextW(IntPtr hWnd, StringBuilder lpString, int nMaxCount);

    [DllImport("user32.dll")]
    public static extern bool EnumWindows(EnumWindowsProc lpEnumFunc, IntPtr lParam);

    [DllImport("user32.dll")]
    public static extern bool IsWindowVisible(IntPtr hWnd);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)]
    public static extern IntPtr GetModuleHandleW(string lpModuleName);

    [DllImport("ole32.dll")]
    public static extern int CoInitializeEx(IntPtr pvReserved, uint dwCoInit);

    public const uint GA_ROOT = 2;

    [DllImport("user32.dll")]
    public static extern IntPtr GetAncestor(IntPtr hWnd, uint gaFlags);

    // Web content reports the render-widget HWND in NativeWindowHandle, not the frame
    // handle GetForegroundWindow returns, so compare Win32 roots instead of raw handles.
    public static IntPtr RootHwnd(IntPtr hwnd)
    {
        if (hwnd == IntPtr.Zero) return IntPtr.Zero;
        return GetAncestor(hwnd, GA_ROOT);
    }

    public static string ClassName(IntPtr hwnd)
    {
        StringBuilder sb = new StringBuilder(256);
        GetClassNameW(hwnd, sb, sb.Capacity);
        return sb.ToString();
    }

    public static string WindowTitle(IntPtr hwnd)
    {
        StringBuilder sb = new StringBuilder(512);
        GetWindowTextW(hwnd, sb, sb.Capacity);
        return sb.ToString();
    }

    public static uint PidOf(IntPtr hwnd)
    {
        uint pid = 0;
        GetWindowThreadProcessId(hwnd, out pid);
        return pid;
    }

    // Chromium reports a synthetic NativeWindowHandle, so the window identity check is
    // the top-level class plus the owning process rather than a handle comparison.
    public static bool IsChromiumWindow(IntPtr hwnd)
    {
        if (hwnd == IntPtr.Zero) return false;
        return ClassName(hwnd).StartsWith("Chrome_WidgetWin", StringComparison.OrdinalIgnoreCase);
    }

    private static bool Down(int vk)
    {
        return (GetAsyncKeyState(vk) & 0x8000) != 0;
    }

    public static bool ModifierDown()
    {
        return Down(VK_SHIFT) || Down(VK_CONTROL) || Down(VK_MENU) || Down(VK_LWIN) || Down(VK_RWIN);
    }

}

internal sealed class Options
{
    public string BridgePath = DefaultBridgePath();
    public int StatusIntervalMs = 5000;
    public string[] Processes = new string[] { "ChatGPT.exe", "Codex Web GPT.exe" };
    public int IntervalMs = 250;
    public int ScanDepth = 2;
    public int DebounceMs = 350;
    public string[] SendLabels = new string[] { "send", "submit", "전송", "보내기" };
    // The exact copy the app renders for a hard block: the disabled Send button's own tooltip and
    // the model-limit banner headline. Broad phrases were dropped because ordinary conversation
    // text can contain them, and a text match must not be able to stand in for a real block.
    public string[] LimitPatterns = new string[]
    {
        "messages limit reached", "usage limit reached", "usage limit for",
        "메시지 한도에 도달", "용량 한도에 도달", "사용 한도에 도달"
    };
    public bool Probe;
    public bool ProbeEnter;
    public bool DryRun;
    public bool Verbose;
    public int Seconds;
    public string LogPath;

    public static Options Parse(string[] args)
    {
        Options o = new Options();
        for (int i = 0; i < args.Length; i++)
        {
            string a = args[i];
            switch (a)
            {
                case "--bridge": o.BridgePath = Next(args, ref i, a); break;
                case "--status-interval": o.StatusIntervalMs = int.Parse(Next(args, ref i, a), CultureInfo.InvariantCulture); break;
                case "--process": o.Processes = Split(Next(args, ref i, a)); break;
                case "--interval": o.IntervalMs = int.Parse(Next(args, ref i, a), CultureInfo.InvariantCulture); break;
                case "--send-label": o.SendLabels = Split(Next(args, ref i, a)); break;
                case "--limit-pattern": o.LimitPatterns = Append(o.LimitPatterns, Split(Next(args, ref i, a))); break;
                case "--scan-depth": o.ScanDepth = int.Parse(Next(args, ref i, a), CultureInfo.InvariantCulture); break;
                case "--log": o.LogPath = Next(args, ref i, a); break;
                case "--seconds": o.Seconds = int.Parse(Next(args, ref i, a), CultureInfo.InvariantCulture); break;
                case "--probe": o.Probe = true; break;
                case "--probe-enter": o.ProbeEnter = true; break;
                case "--dry-run": o.DryRun = true; break;
                case "--debounce": o.DebounceMs = int.Parse(Next(args, ref i, a), CultureInfo.InvariantCulture); break;
                case "--verbose": o.Verbose = true; break;
                case "--help":
                case "-h":
                    Usage();
                    Environment.Exit(0);
                    break;
                default:
                    Console.Error.WriteLine("unknown argument: " + a);
                    Usage();
                    Environment.Exit(2);
                    break;
            }
        }
        return o;
    }

    private static string Next(string[] args, ref int i, string flag)
    {
        if (i + 1 >= args.Length) throw new ArgumentException(flag + " needs a value");
        i++;
        return args[i];
    }

    private static string[] Split(string value)
    {
        string[] parts = value.Split(new char[] { ';', ',' });
        List<string> kept = new List<string>();
        foreach (string p in parts)
        {
            string t = p.Trim();
            if (t.Length > 0) kept.Add(t);
        }
        return kept.ToArray();
    }

    private static string[] Append(string[] existing, string[] added)
    {
        List<string> merged = new List<string>(existing.Length + added.Length);
        merged.AddRange(existing);
        merged.AddRange(added);
        return merged.ToArray();
    }

    private static void Usage()
    {
        Console.WriteLine(@"enter-force-submit [options]
  --bridge <path>          OpenCodex app-server bridge exe, default
                           %LOCALAPPDATA%\OpenCodex\codex-appserver-bridge\ocx-codex-appserver-bridge.exe
  --status-interval <ms>   bridge availability probe interval (default 5000)
  --process <a;b>          target process names (default ""ChatGPT.exe;Codex Web GPT.exe"")
  --interval <ms>          UI Automation poll interval (default 250)
  --send-label <a;b>       exact accessible names treated as the send button
  --limit-pattern <a;b>    extra banner text patterns, added to the built-in list
  --scan-depth <n>         composer-container levels scanned for the banner (default 2)
  --log <path>             log file (default %LOCALAPPDATA%\OpenCodex\enter-force-submit\helper.log)
  --seconds <n>            exit after n seconds
  --dry-run                observe and log, never submit
  --debounce <ms>          wait between the before and after snapshots (default 350)
  --probe                  dump live UI Automation state for target windows and exit
  --probe-enter            run the enter decision sequence and exit (no hook, no submit)
  --verbose                log every poll state change
  --help");
    }

    public bool IsTargetProcess(string name)
    {
        foreach (string p in Processes)
        {
            if (string.Equals(p, name, StringComparison.OrdinalIgnoreCase)) return true;
        }
        return false;
    }

    private static string DefaultBridgePath()
    {
        return Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
            "OpenCodex", "codex-appserver-bridge", "ocx-codex-appserver-bridge.exe");
    }
}

internal sealed class Snapshot
{
    public bool Arm;
    public IntPtr Foreground;
    public uint Pid;
    public string Proc;
    public string Title;
    public string Text;
    public string Reason;

    public static readonly Snapshot Clear = new Snapshot();
}

// Thin client over the OpenCodex app-server bridge CLI. The bridge owns the
// AF_UNIX socket; this helper only runs its exe so a second socket
// implementation cannot drift from the bridge's.
internal sealed class BridgeClient
{
    private const int StatusTimeoutMs = 4000;
    private const int SubmitTimeoutMs = 15000;

    private readonly string executable;
    private readonly int statusIntervalMs;
    private readonly BlockingCollection<Job> queue = new BlockingCollection<Job>();
    private volatile bool available;

    public BridgeClient(string executable, int statusIntervalMs)
    {
        this.executable = executable;
        this.statusIntervalMs = statusIntervalMs;
    }

    public bool Available
    {
        get { return available; }
    }

    public bool Enqueue(string text, Action<bool> onResult)
    {
        if (!available) return false;
        try
        {
            queue.Add(new Job(text, onResult));
            return true;
        }
        catch (Exception)
        {
            return false;
        }
    }

    // One force-submit request plus its completion callback, so the caller learns only from
    // the bridge's own exit code whether the draft actually went through.
    private sealed class Job
    {
        public readonly string Text;
        public readonly Action<bool> OnResult;

        public Job(string text, Action<bool> onResult)
        {
            Text = text;
            OnResult = onResult;
        }
    }

    public void Start()
    {
        Thread status = new Thread(StatusLoop);
        status.IsBackground = true;
        status.Name = "bridge-status";
        status.Start();

        Thread sender = new Thread(SendLoop);
        sender.IsBackground = true;
        sender.Name = "bridge-send";
        sender.Start();
    }

    private void StatusLoop()
    {
        while (true)
        {
            CheckStatus();
            Thread.Sleep(statusIntervalMs < 500 ? 500 : statusIntervalMs);
        }
    }

    private void CheckStatus()
    {
        if (!File.Exists(executable))
        {
            if (available) Log.Warn("bridge exe disappeared: " + executable);
            available = false;
            return;
        }

        string output;
        int exitCode;
        bool ran = Run(new string[] { "--ocx-status" }, StatusTimeoutMs, out output, out exitCode);
        bool now = ran && exitCode == 0;
        if (now != available)
        {
            Log.Info("bridge " + (now ? "available" : "unavailable") + " (exit=" + exitCode + ") " + Clip(output));
        }
        available = now;
    }

    private void SendLoop()
    {
        while (true)
        {
            Job job;
            if (!queue.TryTake(out job, 1000)) continue;

            string output;
            int exitCode;
            bool ran = Run(new string[] { "--ocx-force-submit", job.Text }, SubmitTimeoutMs, out output, out exitCode);
            bool accepted = ran && exitCode == 0;
            if (accepted)
            {
                Log.Info("force-submit accepted: " + Clip(output));
            }
            else
            {
                Log.Warn("force-submit failed (exit=" + exitCode + ", ran=" + ran + "): " + Clip(output));
            }
            if (job.OnResult != null)
            {
                try
                {
                    job.OnResult(accepted);
                }
                catch (Exception)
                {
                }
            }
        }
    }

    private bool Run(string[] arguments, int timeoutMs, out string output, out int exitCode)
    {
        output = "";
        exitCode = -1;
        try
        {
            ProcessStartInfo startInfo = new ProcessStartInfo(executable, JoinArguments(arguments));
            startInfo.UseShellExecute = false;
            startInfo.CreateNoWindow = true;
            startInfo.RedirectStandardOutput = true;
            startInfo.RedirectStandardError = true;
            startInfo.StandardOutputEncoding = new UTF8Encoding(false);
            startInfo.StandardErrorEncoding = new UTF8Encoding(false);

            using (Process process = Process.Start(startInfo))
            {
                if (process == null) return false;

                StringBuilder captured = new StringBuilder();
                process.OutputDataReceived += delegate(object sender, DataReceivedEventArgs e)
                {
                    if (e.Data != null) lock (captured) captured.AppendLine(e.Data);
                };
                process.ErrorDataReceived += delegate(object sender, DataReceivedEventArgs e)
                {
                    if (e.Data != null) lock (captured) captured.AppendLine(e.Data);
                };
                process.BeginOutputReadLine();
                process.BeginErrorReadLine();

                if (!process.WaitForExit(timeoutMs))
                {
                    try { process.Kill(); }
                    catch (Exception) { }
                    output = "timeout after " + timeoutMs + "ms";
                    return false;
                }

                lock (captured) output = captured.ToString();
                exitCode = process.ExitCode;
                return true;
            }
        }
        catch (Exception ex)
        {
            output = ex.Message;
            return false;
        }
    }

    private static string Clip(string value)
    {
        if (value == null) return "";
        string flat = value.Replace("\r", " ").Replace("\n", " ").Trim();
        if (flat.Length <= 300) return flat;
        return flat.Substring(0, 300) + "...";
    }

    // Windows CreateProcess quoting: one argv element per call argument.
    private static string JoinArguments(string[] arguments)
    {
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < arguments.Length; i++)
        {
            if (i > 0) sb.Append(' ');
            sb.Append(QuoteArgument(arguments[i]));
        }
        return sb.ToString();
    }

    private static string QuoteArgument(string value)
    {
        if (value.Length > 0 && value.IndexOfAny(new char[] { ' ', '\t', '\n', '\r', '"' }) < 0) return value;

        StringBuilder sb = new StringBuilder();
        sb.Append('"');
        int backslashes = 0;
        foreach (char c in value)
        {
            if (c == '\\')
            {
                backslashes++;
                continue;
            }
            if (c == '"')
            {
                sb.Append('\\', backslashes * 2 + 1);
                sb.Append('"');
                backslashes = 0;
                continue;
            }
            if (backslashes > 0)
            {
                sb.Append('\\', backslashes);
                backslashes = 0;
            }
            sb.Append(c);
        }
        if (backslashes > 0) sb.Append('\\', backslashes * 2);
        sb.Append('"');
        return sb.ToString();
    }
}

internal static class Log
{
    private static readonly object Gate = new object();
    private static string path;

    public static bool Verbose { get; set; }

    public static void Configure(string configured, bool verbose)
    {
        Verbose = verbose;
        if (configured != null && configured.Length > 0)
        {
            path = configured;
        }
        else
        {
            string root = Path.Combine(
                Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
                "OpenCodex", "enter-force-submit");
            path = Path.Combine(root, "helper.log");
        }
        try
        {
            Directory.CreateDirectory(Path.GetDirectoryName(path));
        }
        catch (Exception)
        {
        }
    }

    public static void Info(string message)
    {
        Write("INFO", message);
    }

    public static void Warn(string message)
    {
        Write("WARN", message);
    }

    public static void Detail(string message)
    {
        if (Verbose) Write("DEBUG", message);
    }

    private static void Write(string level, string message)
    {
        string line = DateTimeOffset.Now.ToString("o", CultureInfo.InvariantCulture) + " " + level + " " + message;
        lock (Gate)
        {
            try
            {
                File.AppendAllText(path, line + Environment.NewLine, new UTF8Encoding(false));
            }
            catch (Exception)
            {
            }
        }
    }
}

internal static class Program
{
    private static Options Opt;
    private static BridgeClient Bridge;
    private static volatile Snapshot Current = Snapshot.Clear;
    private static Native.HookProcDelegate hookProc;
    private static string lastStatus = "";
    // Duplicate-submission guard. lastSubmittedText is written only when the bridge reports
    // success, and is released when the composer empties.
    private static volatile string lastSubmittedText;
    private static volatile bool submitInFlight;
    // Observation queue. The hook only enqueues; a worker thread does every UI Automation call,
    // so the low-level hook never blocks on a cross-process read.
    private static readonly BlockingCollection<EnterObservation> observations = new BlockingCollection<EnterObservation>();
    // The poller publishes the composer snapshot it just resolved and the hook copies it into the
    // queue, so the queued press carries the immutable pre-Enter state without touching UI Automation.
    private static volatile ComposerState cachedState;
    private static int cachedStateTick;

    [MTAThread]
    private static int Main(string[] args)
    {
        try
        {
            Opt = Options.Parse(args);
        }
        catch (Exception ex)
        {
            Console.Error.WriteLine(ex.Message);
            return 2;
        }

        Log.Configure(Opt.LogPath, Opt.Verbose);

        if (Opt.Probe)
        {
            return Probe.Run(Opt);
        }

        if (Opt.ProbeEnter)
        {
            return Probe.RunEnter(Opt);
        }

        bool createdNew;
        using (Mutex single = new Mutex(true, "Local\\OpenCodexEnterForceSubmit", out createdNew))
        {
            if (!createdNew)
            {
                Log.Warn("another enter-force-submit instance already owns the hook; exiting");
                return 1;
            }

            Bridge = new BridgeClient(Opt.BridgePath, Opt.StatusIntervalMs);
            Bridge.Start();

            hookProc = HookCallback;
            IntPtr hook = Native.SetWindowsHookExW(Native.WH_KEYBOARD_LL, hookProc, Native.GetModuleHandleW(null), 0);
            if (hook == IntPtr.Zero)
            {
                Log.Warn("SetWindowsHookExW failed: " + Marshal.GetLastWin32Error());
                return 3;
            }

            Thread poller = new Thread(PollLoop);
            poller.IsBackground = true;
            poller.Name = "uia-poller";
            poller.Start();

            Thread observer = new Thread(ObservationLoop);
            observer.IsBackground = true;
            observer.Name = "enter-observer";
            observer.Start();

            Log.Info("enter-force-submit started (bridge=" + Opt.BridgePath + ", interval=" + Opt.IntervalMs
                + "ms, processes=" + string.Join(",", Opt.Processes) + ", dryRun=" + Opt.DryRun + ")");

            if (Opt.Seconds > 0)
            {
                uint threadId = Native.GetCurrentThreadId();
                int seconds = Opt.Seconds;
                Thread stopper = new Thread(delegate()
                {
                    Thread.Sleep(seconds * 1000);
                    Native.PostThreadMessageW(threadId, Native.WM_QUIT, IntPtr.Zero, IntPtr.Zero);
                });
                stopper.IsBackground = true;
                stopper.Start();
            }

            Native.MSG msg;
            while (Native.GetMessageW(out msg, IntPtr.Zero, 0, 0) > 0)
            {
                Native.TranslateMessage(ref msg);
                Native.DispatchMessageW(ref msg);
            }

            Native.UnhookWindowsHookEx(hook);
            Log.Info("enter-force-submit stopped");
        }
        return 0;
    }

    // Low-level hook body. Every decision is a cached lookup so this returns well
    // inside the Windows low-level hook timeout.
    private static IntPtr HookCallback(int nCode, IntPtr wParam, IntPtr lParam)
    {
        if (nCode >= 0)
        {
            int message = wParam.ToInt32();
            bool down = message == Native.WM_KEYDOWN || message == Native.WM_SYSKEYDOWN;
            bool up = message == Native.WM_KEYUP || message == Native.WM_SYSKEYUP;
            if (down || up)
            {
                Native.KBDLLHOOKSTRUCT key = (Native.KBDLLHOOKSTRUCT)Marshal.PtrToStructure(lParam, typeof(Native.KBDLLHOOKSTRUCT));
                if (key.vkCode == Native.VK_RETURN)
                {
                    if (down && (key.flags & Native.LLKHF_ALTDOWN) == 0 && !Native.ModifierDown()) ObserveEnter();
                }
            }
        }
        return Native.CallNextHookEx(IntPtr.Zero, nCode, wParam, lParam);
    }

    // ---------------------------------------------------------------------------------------
    // Outcome observation
    //
    // A bare Enter is never swallowed. The hook records the press, the poller's cached state is
    // only a cheap "is this worth looking at" filter, and this worker does the real work off the
    // hook thread: resolve the state before the press, wait out a debounce, resolve it again, and
    // submit only when the two resolutions agree and the hard block is still in force.
    // ---------------------------------------------------------------------------------------

    // How old the cached pre-enter snapshot may be before the worker refuses to trust it.
    internal const int MaxCachedAgeMs = 600;

    internal sealed class ComposerState
    {
        public IntPtr Foreground;
        public uint Pid;
        public string Proc;
        public string Title;
        public string ComposerRuntimeId;
        // Draft is the raw UI Automation value, kept exactly as reported so what gets submitted is
        // what the user typed. DraftTrimmed and the placeholder flag exist only for the empty,
        // placeholder and comparison checks.
        public string Draft = "";
        public string DraftTrimmed = "";
        public bool DraftIsPlaceholder;
        public string SendButtonName = "";
        public bool SendButtonFound;
        public bool SendEnabled;
        public bool HardLimit;
        public string TaskFingerprint = "";

        public bool Qualifies
        {
            get
            {
                return SendButtonFound && !DraftIsPlaceholder && DraftTrimmed.Length > 0
                    && !SendEnabled && HardLimit;
            }
        }

        public string QualifyReason()
        {
            if (!SendButtonFound) return "send-button-not-found";
            if (DraftIsPlaceholder) return "composer-only-placeholder";
            if (DraftTrimmed.Length == 0) return "composer-empty";
            if (SendEnabled) return "send-enabled";
            if (!HardLimit) return "no-limit-indicator";
            return "ok";
        }

        // Compares the raw draft and the identity and state fields exactly.
        public bool SameAs(ComposerState other)
        {
            if (other == null) return false;
            return Foreground == other.Foreground
                && Pid == other.Pid
                && string.Equals(ComposerRuntimeId, other.ComposerRuntimeId, StringComparison.Ordinal)
                && string.Equals(Draft, other.Draft, StringComparison.Ordinal)
                && DraftIsPlaceholder == other.DraftIsPlaceholder
                && string.Equals(SendButtonName, other.SendButtonName, StringComparison.Ordinal)
                && SendButtonFound == other.SendButtonFound
                && SendEnabled == other.SendEnabled
                && HardLimit == other.HardLimit
                && string.Equals(TaskFingerprint, other.TaskFingerprint, StringComparison.Ordinal);
        }

        public ComposerState Copy()
        {
            ComposerState copy = new ComposerState();
            copy.Foreground = Foreground;
            copy.Pid = Pid;
            copy.Proc = Proc;
            copy.Title = Title;
            copy.ComposerRuntimeId = ComposerRuntimeId;
            copy.Draft = Draft;
            copy.DraftTrimmed = DraftTrimmed;
            copy.DraftIsPlaceholder = DraftIsPlaceholder;
            copy.SendButtonName = SendButtonName;
            copy.SendButtonFound = SendButtonFound;
            copy.SendEnabled = SendEnabled;
            copy.HardLimit = HardLimit;
            copy.TaskFingerprint = TaskFingerprint;
            return copy;
        }

        public string Describe()
        {
            return "pid=" + Pid.ToString(CultureInfo.InvariantCulture)
                + " title=\"" + Title + "\""
                + " task=\"" + TaskFingerprint + "\""
                + " composer=" + ComposerRuntimeId
                + " draft=\"" + Clip(Draft, 60) + "\" (raw " + Draft.Length.ToString(CultureInfo.InvariantCulture)
                + " chars, placeholder=" + DraftIsPlaceholder + ")"
                + " send=\"" + SendButtonName + "\" enabled=" + SendEnabled
                + " hardLimit=" + HardLimit;
        }
    }

    private sealed class EnterObservation
    {
        public IntPtr Foreground;
        public int Tick;
        public ComposerState Cached;
        public int CachedTick;
    }

    private static void ObserveEnter()
    {
        Snapshot armed = Current;
        if (armed == null || !armed.Arm) return;
        ComposerState snapshot = cachedState;
        if (snapshot == null) return;
        IntPtr foreground = Native.GetForegroundWindow();
        if (foreground != armed.Foreground) return;

        EnterObservation observation = new EnterObservation();
        observation.Foreground = foreground;
        observation.Tick = Environment.TickCount;
        observation.Cached = snapshot.Copy();
        observation.CachedTick = cachedStateTick;
        if (!observations.TryAdd(observation))
        {
            Log.Warn("enter observation queue is full; dropping this press");
        }
    }

    private static void ObservationLoop()
    {
        Native.CoInitializeEx(IntPtr.Zero, Native.COINIT_MULTITHREADED);
        while (true)
        {
            EnterObservation observation;
            if (!observations.TryTake(out observation, 500)) continue;
            try
            {
                Observe(observation);
            }
            catch (Exception ex)
            {
                Log.Warn("observation failed: " + ex.Message);
            }
        }
    }

    private static void Observe(EnterObservation observation)
    {
        // Age of the cached pre-enter snapshot, measured before the debounce so the bound describes
        // how fresh the pre-Enter state was rather than how long the deliberate wait lasts.
        int cachedAgeMs = unchecked(Environment.TickCount - observation.CachedTick);
        if (observation.Cached != null) Log.Detail("pre-enter snapshot: " + observation.Cached.Describe());

        Thread.Sleep(Opt.DebounceMs);

        string detail;
        ComposerState after = ResolveComposerState(observation.Foreground, out detail);
        bool submit = DecideObservation(observation.Cached, after, cachedAgeMs, lastSubmittedText, out detail);
        if (!submit)
        {
            Log.Info("no submission: " + detail);
            return;
        }
        if (Opt.DryRun)
        {
            Log.Info("dry-run: would force-submit " + detail + ": " + after.Describe());
            return;
        }
        if (submitInFlight)
        {
            Log.Info("no submission: a force-submit is already in flight");
            return;
        }
        if (!Bridge.Available)
        {
            Log.Warn("no submission: the bridge is not available");
            return;
        }

        // Immediate final fresh snapshot: it must still qualify and still match the post-press read.
        ComposerState final = ResolveComposerState(observation.Foreground, out detail);
        if (final == null || !final.Qualifies || !after.SameAs(final))
        {
            Log.Warn("no submission: the final recheck disagreed (" + detail + ")");
            return;
        }

        Dispatch(final, detail);
    }

    // Decision core, shared by the observation loop and the --probe-enter sequence so the
    // reported behavior is the behavior that runs. It judges outcomes only: neither the draft's
    // text nor the active input language is a signal, and anything that does not match exactly is
    // treated as ambiguous and left alone.
    internal static bool DecideObservation(ComposerState cached, ComposerState after, int cachedAgeMs, string recorded, out string detail)
    {
        if (cached == null)
        {
            detail = "no pre-enter snapshot was cached";
            return false;
        }
        if (cachedAgeMs > MaxCachedAgeMs)
        {
            detail = "the pre-enter snapshot was stale (" + cachedAgeMs.ToString(CultureInfo.InvariantCulture) + "ms)";
            return false;
        }
        if (!cached.Qualifies)
        {
            detail = "the pre-enter snapshot did not qualify: " + cached.QualifyReason();
            return false;
        }
        if (after == null)
        {
            detail = "the state could not be resolved after the press";
            return false;
        }
        if (!after.Qualifies)
        {
            detail = "the post-press snapshot does not qualify: " + after.QualifyReason();
            return false;
        }
        if (!cached.SameAs(after))
        {
            // Covers a normal send (the composer cleared), a composition commit or any other edit
            // that altered the draft, a turn starting (the send control turns into a stop control),
            // and a task or focus switch.
            detail = "the state changed after the press, leaving it to the app";
            return false;
        }
        if (recorded != null && string.Equals(after.Draft, recorded, StringComparison.Ordinal))
        {
            detail = "this draft was already submitted";
            return false;
        }
        detail = "unchanged hard block, nothing else touched the composer";
        return true;
    }

    private static void Dispatch(ComposerState state, string detail)
    {
        string payload = state.Draft;
        submitInFlight = true;
        if (!Bridge.Enqueue(payload, delegate(bool accepted)
        {
            submitInFlight = false;
            if (accepted)
            {
                lastSubmittedText = payload;
                Log.Info("force-submit accepted; draft recorded as submitted");
            }
            else
            {
                Log.Warn("force-submit was not accepted; the same draft may be retried");
            }
        }))
        {
            submitInFlight = false;
            Log.Warn("bridge rejected the submit; nothing was sent");
            return;
        }

        Log.Info("force-submit dispatched (" + detail + "): " + state.Describe());
    }

    // Live resolution of the whole chain. Null means this is not our window or the focused
    // element is not a composer at all. Otherwise the state carries whatever was found and
    // Qualifies decides whether this is the hard-block situation worth acting on.
    internal static ComposerState ResolveComposerState(IntPtr foreground, out string detail)
    {
        detail = null;
        if (foreground == IntPtr.Zero)
        {
            detail = "no foreground window";
            return null;
        }
        if (!Native.IsChromiumWindow(foreground))
        {
            detail = "foreground is not a chromium window";
            return null;
        }

        uint pid = Native.PidOf(foreground);
        string proc = ProcessName(pid);
        if (!Opt.IsTargetProcess(proc))
        {
            detail = "foreground process is not a target: " + proc;
            return null;
        }

        AutomationElement focused;
        try
        {
            focused = AutomationElement.FocusedElement;
        }
        catch (Exception)
        {
            detail = "focused element unavailable";
            return null;
        }
        if (focused == null)
        {
            detail = "no focused element";
            return null;
        }

        int elementPid;
        try
        {
            elementPid = focused.Current.ProcessId;
        }
        catch (Exception)
        {
            detail = "focused element process unavailable";
            return null;
        }
        if ((uint)elementPid != pid)
        {
            detail = "focus is in another process";
            return null;
        }

        ControlType controlType;
        try
        {
            controlType = focused.Current.ControlType;
        }
        catch (Exception)
        {
            detail = "focused control type unavailable";
            return null;
        }
        if (controlType != ControlType.Edit)
        {
            detail = "focused element is not a composer: " + controlType.ProgrammaticName;
            return null;
        }

        string text = ReadValue(focused);
        if (text == null)
        {
            detail = "the composer exposes no value";
            return null;
        }

        ComposerState state = new ComposerState();
        state.Foreground = foreground;
        state.Pid = pid;
        state.Proc = proc;
        state.Title = Native.WindowTitle(foreground);
        state.ComposerRuntimeId = RuntimeId(focused);
        state.TaskFingerprint = TaskFingerprint(focused);

        // The raw value is kept verbatim because it is what gets submitted. Chromium reports an
        // empty ProseMirror composer as its placeholder text, so a value whose trimmed form repeats
        // the element's accessible name counts as empty rather than as a draft.
        state.Draft = text;
        state.DraftTrimmed = text.Trim();
        string accessibleName = SafeName(focused).Trim();
        state.DraftIsPlaceholder = state.DraftTrimmed.Length == 0
            || (accessibleName.Length > 0 && string.Equals(state.DraftTrimmed, accessibleName, StringComparison.Ordinal));

        int sendDepth;
        AutomationElement sendButton = FindSendButton(focused, out sendDepth);
        if (sendButton == null)
        {
            state.SendButtonFound = false;
            return state;
        }
        state.SendButtonFound = true;
        state.SendButtonName = SafeName(sendButton);
        state.SendEnabled = SafeEnabled(sendButton);
        // The limit scan is the expensive part and an enabled send button already disqualifies the
        // state, so it is skipped there.
        state.HardLimit = state.SendEnabled ? false : LimitIndicatorLive(sendButton, focused);
        return state;
    }

    private static string RuntimeId(AutomationElement element)
    {
        try
        {
            int[] runtimeId = element.GetRuntimeId();
            if (runtimeId == null || runtimeId.Length == 0) return "";
            StringBuilder sb = new StringBuilder();
            foreach (int part in runtimeId)
            {
                if (sb.Length > 0) sb.Append('.');
                sb.Append(part.ToString(CultureInfo.InvariantCulture));
            }
            return sb.ToString();
        }
        catch (Exception)
        {
            return "";
        }
    }

    // Weak task evidence: the nearest document ancestor's accessible name, which the app fills
    // with the active conversation. A task switch therefore fails the before/after comparison.
    private static string TaskFingerprint(AutomationElement element)
    {
        AutomationElement node = element;
        for (int depth = 0; depth < 12; depth++)
        {
            node = SafeParent(node);
            if (node == null) return "";
            try
            {
                if (node.Current.ControlType == ControlType.Document) return Clip(SafeName(node), 80);
            }
            catch (Exception)
            {
                return "";
            }
        }
        return "";
    }

    internal static string Clip(string value, int limit)
    {
        if (value == null) return "";
        if (value.Length <= limit) return value;
        return value.Substring(0, limit) + "...";
    }

    private static void PollLoop()
    {
        Native.CoInitializeEx(IntPtr.Zero, Native.COINIT_MULTITHREADED);
        while (true)
        {
            try
            {
                PollOnce();
            }
            catch (Exception ex)
            {
                SetState(Snapshot.Clear, "poll-error: " + ex.Message);
            }
            Thread.Sleep(Opt.IntervalMs);
        }
    }

    private static void PollOnce()
    {
        IntPtr foreground = Native.GetForegroundWindow();
        if (foreground == IntPtr.Zero)
        {
            SetState(Snapshot.Clear, "no-foreground");
            return;
        }

        uint pid = Native.PidOf(foreground);
        string proc = ProcessName(pid);
        if (!Opt.IsTargetProcess(proc))
        {
            SetState(Snapshot.Clear, "foreground-not-target:" + proc);
            return;
        }

        if (!Opt.DryRun && !Bridge.Available)
        {
            SetState(Snapshot.Clear, "bridge-unavailable");
            return;
        }

        string detail;
        ComposerState state = ResolveComposerState(foreground, out detail);
        if (state == null)
        {
            SetState(Snapshot.Clear, detail);
            return;
        }

        if (state.DraftIsPlaceholder)
        {
            // An emptied composer releases the duplicate guard, so the same text can go again.
            lastSubmittedText = null;
        }

        if (!state.Qualifies)
        {
            SetState(Snapshot.Clear, state.QualifyReason());
            return;
        }

        if (submitInFlight)
        {
            SetState(Snapshot.Clear, "submit-in-flight");
            return;
        }

        string recorded = lastSubmittedText;
        if (recorded != null && string.Equals(state.Draft, recorded, StringComparison.Ordinal))
        {
            SetState(Snapshot.Clear, "draft-already-submitted");
            return;
        }

        // Publish the immutable pre-enter snapshot the hook copies into the observation queue.
        cachedState = state.Copy();
        cachedStateTick = Environment.TickCount;

        Snapshot armed = new Snapshot();
        armed.Arm = true;
        armed.Foreground = foreground;
        armed.Pid = state.Pid;
        armed.Proc = state.Proc;
        armed.Title = state.Title;
        armed.Text = state.Draft;
        armed.Reason = Opt.DryRun ? "armed-dry-run" : "armed";
        SetState(armed, armed.Reason + " (rawChars=" + state.Draft.Length.ToString(CultureInfo.InvariantCulture)
            + " send=\"" + state.SendButtonName + "\")");
    }

    private static void SetState(Snapshot next, string status)
    {
        Current = next;
        if (status != lastStatus)
        {
            lastStatus = status;
            Log.Detail("state: " + status);
        }
    }

    private static string ProcessName(uint pid)
    {
        try
        {
            return Process.GetProcessById((int)pid).ProcessName + ".exe";
        }
        catch (Exception)
        {
            return "?";
        }
    }

    internal static AutomationElement SafeParent(AutomationElement element)
    {
        try
        {
            return TreeWalker.ControlViewWalker.GetParent(element);
        }
        catch (Exception)
        {
            return null;
        }
    }

    internal static IntPtr SafeNativeWindow(AutomationElement element)
    {
        try
        {
            return (IntPtr)element.Current.NativeWindowHandle;
        }
        catch (Exception)
        {
            return IntPtr.Zero;
        }
    }

    internal static IntPtr TopLevelWindow(AutomationElement element)
    {
        AutomationElement node = element;
        for (int i = 0; i < 48; i++)
        {
            AutomationElement parent = SafeParent(node);
            if (parent == null) break;
            node = parent;
        }
        return SafeNativeWindow(node);
    }

    internal static string SafeName(AutomationElement element)
    {
        try
        {
            return element.Current.Name;
        }
        catch (Exception)
        {
            return "";
        }
    }

    internal static string SafeAutomationId(AutomationElement element)
    {
        try
        {
            return element.Current.AutomationId;
        }
        catch (Exception)
        {
            return "";
        }
    }

    internal static string SafeClassName(AutomationElement element)
    {
        try
        {
            return element.Current.ClassName;
        }
        catch (Exception)
        {
            return "";
        }
    }

    internal static bool SafeEnabled(AutomationElement element)
    {
        try
        {
            return element.Current.IsEnabled;
        }
        catch (Exception)
        {
            return true;
        }
    }

    internal static string SafeHelpText(AutomationElement element)
    {
        try
        {
            return element.Current.HelpText;
        }
        catch (Exception)
        {
            return "";
        }
    }

    internal static string ReadValue(AutomationElement element)
    {
        object pattern;
        try
        {
            if (!element.TryGetCurrentPattern(ValuePattern.Pattern, out pattern)) return null;
        }
        catch (Exception)
        {
            return null;
        }
        try
        {
            return ((ValuePattern)pattern).Current.Value;
        }
        catch (Exception)
        {
            return null;
        }
    }

    internal static AutomationElementCollection FindDescendants(AutomationElement element, ControlType type)
    {
        try
        {
            return element.FindAll(TreeScope.Descendants, new PropertyCondition(AutomationElement.ControlTypeProperty, type));
        }
        catch (Exception)
        {
            return null;
        }
    }

    // The composer container has no stable identifier, so climb from the focused edit
    // control and take the first button carrying a send label, within a few levels.
    // There is deliberately no positional fallback: a transcript helper button picked by
    // position is worse than no match, because no match leaves Enter exactly as it was.
    internal const int MaxSendButtonDepth = 4;

    private static AutomationElement FindSendButton(AutomationElement composer, out int foundDepth)
    {
        foundDepth = -1;
        AutomationElement node = composer;
        for (int depth = 0; depth <= MaxSendButtonDepth && node != null; depth++)
        {
            AutomationElementCollection buttons = FindDescendants(node, ControlType.Button);
            if (buttons != null)
            {
                foreach (AutomationElement button in buttons)
                {
                    if (IsSendLabel(SafeName(button)))
                    {
                        foundDepth = depth;
                        return button;
                    }
                }
            }
            node = SafeParent(node);
        }
        return null;
    }

    internal static bool IsSendLabel(string name)
    {
        if (name == null) return false;
        string trimmed = name.Trim();
        if (trimmed.Length == 0) return false;
        foreach (string label in Opt.SendLabels)
        {
            if (string.Equals(trimmed, label.Trim(), StringComparison.OrdinalIgnoreCase)) return true;
        }
        return false;
    }

    // A composer region holds a handful of text nodes; the conversation below it holds
    // tens of thousands. Levels over the cap are skipped instead of walked.
    internal const int MaxTextNodesPerLevel = 400;

    // Strongest signal first: in the hard-block state the app hangs its Messages limit reached
    // tooltip on the disabled Send button. Otherwise scan outward from the composer container,
    // capped at ScanDepth levels, and skip any level too large to be the composer.
    private static bool LimitIndicatorLive(AutomationElement sendButton, AutomationElement composer)
    {
        bool found = false;

        // Strongest signal: in the hard-block state the app hangs its Messages limit
        // reached tooltip on the disabled Send button itself.
        string help = SafeHelpText(sendButton);
        if (MatchesLimitPattern(help))
        {
            found = true;
            Log.Detail("limit indicator (send button help text): " + help);
        }

        AutomationElement node = SafeParent(sendButton);
        if (node == null) node = composer;
        for (int depth = 0; depth < Opt.ScanDepth && node != null && !found; depth++)
        {
            AutomationElementCollection texts = FindDescendants(node, ControlType.Text);
            if (texts != null && texts.Count <= MaxTextNodesPerLevel)
            {
                foreach (AutomationElement text in texts)
                {
                    string name = SafeName(text);
                    if (MatchesLimitPattern(name))
                    {
                        found = true;
                        Log.Detail("limit indicator: " + name);
                        break;
                    }
                }
            }
            else if (texts != null)
            {
                Log.Detail("limit scan skipped a level with " + texts.Count + " text nodes");
            }
            node = SafeParent(node);
        }

        return found;
    }

    internal static bool MatchesLimitPattern(string name)
    {
        if (name == null || name.Length == 0) return false;
        string lower = name.ToLowerInvariant();
        foreach (string pattern in Opt.LimitPatterns)
        {
            if (lower.Contains(pattern.ToLowerInvariant())) return true;
        }
        return false;
    }
}

internal static class Probe
{
    public static int Run(Options opt)
    {
        Console.WriteLine("=== enter-force-submit probe ===");
        Console.WriteLine("target processes : " + string.Join(", ", opt.Processes));
        Console.WriteLine("bridge            : " + opt.BridgePath + " (exists=" + File.Exists(opt.BridgePath) + ")");
        Console.WriteLine("limit patterns    : " + string.Join(" | ", opt.LimitPatterns));

        IntPtr foreground = Native.GetForegroundWindow();
        Console.WriteLine();
        Console.WriteLine("foreground        : hwnd=0x" + foreground.ToString("X")
            + " class=" + Native.ClassName(foreground)
            + " proc=" + ProcessName(Native.PidOf(foreground))
            + " title=\"" + Native.WindowTitle(foreground) + "\"");
        Console.WriteLine("modifiers down    : " + Native.ModifierDown());

        List<IntPtr> windows = new List<IntPtr>();
        Native.EnumWindows(delegate(IntPtr hwnd, IntPtr state)
        {
            if (!Native.IsWindowVisible(hwnd)) return true;
            if (!opt.IsTargetProcess(ProcessName(Native.PidOf(hwnd)))) return true;
            windows.Add(hwnd);
            return true;
        }, IntPtr.Zero);

        if (windows.Count == 0)
        {
            Console.WriteLine();
            Console.WriteLine("no visible target window found. Is the Codex desktop app running?");
            return 1;
        }

        foreach (IntPtr hwnd in windows)
        {
            DumpWindow(hwnd);
        }

        Console.WriteLine();
        Console.WriteLine("=== probe end ===");
        return 0;
    }

    // Targeted probe of the enter state machine. It resolves the live state through the same
    // capture the observation loop uses, then drives the decision core over the situations the
    // app can present. No hook is installed and nothing is submitted.
    public static int RunEnter(Options opt)
    {
        Console.WriteLine("=== enter decision probe ===");
        IntPtr foreground = Native.GetForegroundWindow();
        Console.WriteLine("foreground       : 0x" + foreground.ToString("X") + " " + Native.ClassName(foreground)
            + " proc=" + ProcessName(Native.PidOf(foreground)));
        Console.WriteLine("timing           : debounce=" + opt.DebounceMs + "ms, maxCachedAge="
            + Program.MaxCachedAgeMs + "ms");

        string detail;
        Program.ComposerState live = Program.ResolveComposerState(foreground, out detail);
        Console.WriteLine();
        Console.WriteLine("live capture     : " + (live == null ? "<unavailable: " + detail + ">" : live.Describe()));
        if (live != null)
        {
            Console.WriteLine("live qualifies   : " + live.Qualifies + " (" + live.QualifyReason() + ")");
        }

        Console.WriteLine();
        DumpComposerExposure(opt.ScanDepth);

        Console.WriteLine();
        Console.WriteLine("scripted sequence through the same decision core:");
        IntPtr window = foreground == IntPtr.Zero ? (IntPtr)0x1 : foreground;
        Program.ComposerState blocked = State(window, "draft A", "보내기", false, true, "task-1");

        Step(1, "hard block, unchanged, fresh pre-enter snapshot", blocked, blocked, 40, null,
            "submit: unchanged hard block, nothing else touched the composer");
        Step(2, "the draft changed after the press", blocked,
            State(window, "draft AB", "보내기", false, true, "task-1"), 40, null,
            "pass: the state changed after the press, leaving it to the app");
        Step(3, "a normal send cleared the composer", blocked,
            State(window, "", "보내기", false, true, "task-1"), 40, null,
            "pass: the post-press snapshot does not qualify: composer-only-placeholder");
        Step(4, "a turn began, the control became stop", blocked,
            State(window, "draft A", "중지", false, true, "task-1"), 40, null,
            "pass: the state changed after the press, leaving it to the app");
        Step(5, "the hard block ended", blocked,
            State(window, "draft A", "보내기", true, false, "task-1"), 40, null,
            "pass: the post-press snapshot does not qualify: send-enabled");
        Step(6, "the task switched", blocked,
            State(window, "draft A", "보내기", false, true, "task-2"), 40, null,
            "pass: the state changed after the press, leaving it to the app");
        Step(7, "focus moved to another composer", blocked,
            DifferentComposer(window, "draft A"), 40, null,
            "pass: the state changed after the press, leaving it to the app");
        Step(8, "the pre-enter snapshot is stale", blocked, blocked, Program.MaxCachedAgeMs + 300, null,
            "pass: the pre-enter snapshot was stale (" + (Program.MaxCachedAgeMs + 300) + "ms)");
        Step(9, "no pre-enter snapshot was cached", null, blocked, 40, null,
            "pass: no pre-enter snapshot was cached");
        Step(10, "the pre-enter snapshot did not qualify", PlaceholderState(window), PlaceholderState(window), 40, null,
            "pass: the pre-enter snapshot did not qualify: composer-only-placeholder");
        Step(11, "the post-press snapshot does not qualify", blocked,
            State(window, "draft A", "보내기", true, false, "task-1"), 40, null,
            "pass: the post-press snapshot does not qualify: send-enabled");
        Step(12, "draft already submitted", blocked, blocked, 40, "draft A",
            "pass: this draft was already submitted");
        Step(13, "the after snapshot is unavailable", blocked, null, 40, null,
            "pass: the state could not be resolved after the press");

        Console.WriteLine();
        Console.WriteLine("=== probe end ===");
        return 0;
    }

    private static Program.ComposerState State(IntPtr window, string draft, string sendName, bool sendEnabled, bool hardLimit, string task)
    {
        Program.ComposerState state = new Program.ComposerState();
        state.Foreground = window;
        state.Pid = 0x1234;
        state.Proc = "ChatGPT.exe";
        state.Title = "ChatGPT";
        state.ComposerRuntimeId = "42.1.2";
        state.Draft = draft;
        state.DraftTrimmed = draft.Trim();
        state.DraftIsPlaceholder = state.DraftTrimmed.Length == 0;
        state.SendButtonName = sendName;
        state.SendButtonFound = true;
        state.SendEnabled = sendEnabled;
        state.HardLimit = hardLimit;
        state.TaskFingerprint = task;
        return state;
    }

    private static Program.ComposerState DifferentComposer(IntPtr window, string draft)
    {
        Program.ComposerState state = State(window, draft, "보내기", false, true, "task-1");
        state.ComposerRuntimeId = "42.9.9";
        return state;
    }

    // A composer whose value is only the accessible placeholder, which is what Chromium reports for
    // an empty ProseMirror composer.
    private static Program.ComposerState PlaceholderState(IntPtr window)
    {
        Program.ComposerState state = State(window, " ChatGPT placeholder", "보내기", false, true, "task-1");
        state.DraftTrimmed = "ChatGPT placeholder";
        state.DraftIsPlaceholder = true;
        return state;
    }

    // Read-only report of what the helper can see in the live composer: the raw draft, every button
    // it would consider as the send control, the nearby text matching a limit phrase, and an honest
    // verdict on whether the hard block is active. Nothing is typed and nothing is changed.
    private static void DumpComposerExposure(int scanDepth)
    {
        Console.WriteLine("composer exposure (read-only):");
        AutomationElement composer = null;
        try
        {
            composer = AutomationElement.FocusedElement;
        }
        catch (Exception)
        {
        }
        if (composer == null)
        {
            Console.WriteLine("  focused element is unavailable");
            return;
        }

        Console.WriteLine("  focused        : " + TypeName(composer)
            + " name=\"" + Clip(Program.SafeName(composer)) + "\"");
        if (TypeName(composer) != "Edit")
        {
            Console.WriteLine("  note           : the focused element is not the composer edit, so no send button or limit scan applies");
            return;
        }

        string raw = Program.ReadValue(composer);
        if (raw == null)
        {
            Console.WriteLine("  draft          : <the composer exposes no value>");
            return;
        }
        string accessibleName = Program.SafeName(composer).Trim();
        string trimmed = raw.Trim();
        bool placeholder = trimmed.Length == 0
            || (accessibleName.Length > 0 && string.Equals(trimmed, accessibleName, StringComparison.Ordinal));
        Console.WriteLine("  draft raw      : \"" + Clip(raw) + "\"");
        Console.WriteLine("                   rawLen=" + raw.Length.ToString(CultureInfo.InvariantCulture)
            + " trimmedLen=" + trimmed.Length.ToString(CultureInfo.InvariantCulture)
            + " placeholder=" + placeholder);
        if (trimmed.Length == 0 || placeholder)
        {
            Console.WriteLine("  note           : there is no nonempty draft, so the app renders no send control to read.");
            Console.WriteLine("                   type a draft and rerun --probe-enter to read the send button and limit exposure.");
        }

        Console.WriteLine("  send candidates:");
        AutomationElement node = composer;
        for (int depth = 0; depth <= Program.MaxSendButtonDepth && node != null; depth++)
        {
            AutomationElementCollection buttons = Program.FindDescendants(node, ControlType.Button);
            if (buttons != null)
            {
                int shown = 0;
                foreach (AutomationElement button in buttons)
                {
                    if (shown++ >= 12) break;
                    string name = Program.SafeName(button);
                    Console.WriteLine("    depth=" + depth.ToString(CultureInfo.InvariantCulture)
                        + " name=\"" + Clip(name) + "\" enabled=" + Program.SafeEnabled(button)
                        + " help=\"" + Clip(Program.SafeHelpText(button)) + "\" labelMatch=" + Program.IsSendLabel(name));
                }
            }
            node = Program.SafeParent(node);
        }

        Console.WriteLine("  limit phrases  :");
        int matches = 0;
        node = composer;
        for (int depth = 0; depth < scanDepth && node != null; depth++)
        {
            AutomationElementCollection texts = Program.FindDescendants(node, ControlType.Text);
            if (texts != null && texts.Count <= Program.MaxTextNodesPerLevel)
            {
                foreach (AutomationElement text in texts)
                {
                    string name = Program.SafeName(text);
                    if (Program.MatchesLimitPattern(name))
                    {
                        matches++;
                        Console.WriteLine("    level=" + depth.ToString(CultureInfo.InvariantCulture)
                            + " \"" + Clip(name) + "\"");
                    }
                }
            }
            node = Program.SafeParent(node);
        }
        if (matches == 0)
        {
            Console.WriteLine("    none within " + scanDepth.ToString(CultureInfo.InvariantCulture) + " levels of the composer");
        }

        string detail;
        Program.ComposerState state = Program.ResolveComposerState(Native.GetForegroundWindow(), out detail);
        if (state == null)
        {
            Console.WriteLine("  hard block     : cannot be judged (" + detail + ")");
            return;
        }
        bool blocked = state.SendButtonFound && !state.SendEnabled && state.HardLimit;
        Console.WriteLine("  hard block     : " + (blocked ? "ACTIVE" : "NOT ACTIVE")
            + " (" + state.QualifyReason() + ", send=\"" + state.SendButtonName + "\")");
    }

    private static void Step(int index, string label, Program.ComposerState cached, Program.ComposerState after, int cachedAgeMs, string recorded, string expected)
    {
        string detail;
        bool submit = Program.DecideObservation(cached, after, cachedAgeMs, recorded, out detail);
        string actual = (submit ? "submit: " : "pass: ") + detail;
        string verdict = string.Equals(actual, expected, StringComparison.Ordinal) ? "as-expected" : "UNEXPECTED";
        Console.WriteLine("  " + index.ToString("00", CultureInfo.InvariantCulture) + " " + label);
        Console.WriteLine("       -> " + actual + "  [" + verdict + "]");
    }

    private static void DumpWindow(IntPtr hwnd)
    {
        Console.WriteLine();
        Console.WriteLine("--- window 0x" + hwnd.ToString("X") + " ---");
        Console.WriteLine("class=" + Native.ClassName(hwnd) + " proc=" + ProcessName(Native.PidOf(hwnd)) + " pid=" + Native.PidOf(hwnd));
        Console.WriteLine("title=\"" + Native.WindowTitle(hwnd) + "\"");

        AutomationElement root;
        try
        {
            root = AutomationElement.FromHandle(hwnd);
        }
        catch (Exception ex)
        {
            Console.WriteLine("AutomationElement.FromHandle failed: " + ex.Message);
            return;
        }
        if (root == null)
        {
            Console.WriteLine("AutomationElement.FromHandle returned null");
            return;
        }

        AutomationElement focused = null;
        try
        {
            focused = AutomationElement.FocusedElement;
        }
        catch (Exception ex)
        {
            Console.WriteLine("FocusedElement failed: " + ex.Message);
        }

        if (focused != null)
        {
            Console.WriteLine("focused           : type=" + TypeName(focused)
                + " name=\"" + Program.SafeName(focused) + "\""
                + " autoid=\"" + Program.SafeAutomationId(focused) + "\""
                + " class=\"" + Program.SafeClassName(focused) + "\""
                + " enabled=" + Program.SafeEnabled(focused)
                + " value=\"" + Clip(Program.ReadValue(focused)) + "\"");
            IntPtr elementHandle = Program.SafeNativeWindow(focused);
            IntPtr elementTop = Program.TopLevelWindow(focused);
            Console.WriteLine("focused window    : 0x" + elementHandle.ToString("X")
                + " root=0x" + Native.RootHwnd(elementHandle).ToString("X")
                + " topLevel=0x" + elementTop.ToString("X")
                + " topLevelRoot=0x" + Native.RootHwnd(elementTop).ToString("X")
                + " foregroundRoot=0x" + Native.RootHwnd(Native.GetForegroundWindow()).ToString("X"));
            Console.WriteLine("ancestor chain    : " + Ancestors(focused));
            Console.WriteLine("send button       : " + DescribeSendButton(focused));
        }

        AutomationElementCollection edits = Program.FindDescendants(root, ControlType.Edit);
        Console.WriteLine("edit controls     : " + (edits == null ? "?" : edits.Count.ToString(CultureInfo.InvariantCulture)));
        int editShown = 0;
        if (edits != null)
        {
            foreach (AutomationElement edit in edits)
            {
                if (editShown++ >= 8) break;
                Console.WriteLine("  edit name=\"" + Program.SafeName(edit) + "\""
                    + " autoid=\"" + Program.SafeAutomationId(edit) + "\""
                    + " class=\"" + Program.SafeClassName(edit) + "\""
                    + " enabled=" + Program.SafeEnabled(edit)
                    + " value=\"" + Clip(Program.ReadValue(edit)) + "\"");
            }
        }

        AutomationElementCollection buttons = Program.FindDescendants(root, ControlType.Button);
        Console.WriteLine("buttons           : " + (buttons == null ? "?" : buttons.Count.ToString(CultureInfo.InvariantCulture)));
        int buttonShown = 0;
        if (buttons != null)
        {
            foreach (AutomationElement button in buttons)
            {
                if (buttonShown++ >= 30) break;
                Console.WriteLine("  button name=\"" + Program.SafeName(button) + "\""
                    + " autoid=\"" + Program.SafeAutomationId(button) + "\""
                    + " class=\"" + Program.SafeClassName(button) + "\""
                    + " enabled=" + Program.SafeEnabled(button));
            }
        }

        AutomationElementCollection texts = Program.FindDescendants(root, ControlType.Text);
        Console.WriteLine("text nodes        : " + (texts == null ? "?" : texts.Count.ToString(CultureInfo.InvariantCulture)));
        if (texts != null)
        {
            int matched = 0;
            int shown = 0;
            foreach (AutomationElement text in texts)
            {
                string name = Program.SafeName(text);
                if (Program.MatchesLimitPattern(name))
                {
                    matched++;
                    if (shown++ < 12) Console.WriteLine("  LIMIT-MATCH \"" + Clip(name) + "\"");
                }
            }
            Console.WriteLine("limit matches     : " + matched);
        }
    }

    private static string DescribeSendButton(AutomationElement composer)
    {
        AutomationElement node = composer;
        for (int depth = 0; depth < 8 && node != null; depth++)
        {
            AutomationElementCollection buttons = Program.FindDescendants(node, ControlType.Button);
            if (buttons != null && buttons.Count > 0)
            {
                StringBuilder sb = new StringBuilder();
                sb.Append("depth=").Append(depth).Append(" count=").Append(buttons.Count).Append(" [");
                int shown = 0;
                foreach (AutomationElement button in buttons)
                {
                    if (shown++ >= 10) break;
                    sb.Append("\"").Append(Program.SafeName(button)).Append("\" enabled=")
                        .Append(Program.SafeEnabled(button)).Append(" help=\"").Append(Clip(Program.SafeHelpText(button)))
                        .Append("\"; ");
                }
                sb.Append(']');
                return sb.ToString();
            }
            node = Program.SafeParent(node);
        }
        return "not found";
    }

    private static string Ancestors(AutomationElement element)
    {
        StringBuilder sb = new StringBuilder();
        AutomationElement node = element;
        for (int i = 0; i < 10 && node != null; i++)
        {
            node = Program.SafeParent(node);
            if (node == null) break;
            sb.Append(" > ").Append(TypeName(node));
            string name = Program.SafeName(node);
            if (name != null && name.Length > 0) sb.Append("(\"").Append(Clip(name)).Append("\")");
        }
        return sb.ToString();
    }

    private static string TypeName(AutomationElement element)
    {
        try
        {
            return element.Current.ControlType.ProgrammaticName.Replace("ControlType.", "");
        }
        catch (Exception)
        {
            return "?";
        }
    }

    private static string Clip(string value)
    {
        if (value == null) return "<null>";
        string flat = value.Replace("\r", " ").Replace("\n", " ");
        if (flat.Length <= 80) return flat;
        return flat.Substring(0, 80) + "...";
    }

    private static string ProcessName(uint pid)
    {
        try
        {
            return Process.GetProcessById((int)pid).ProcessName + ".exe";
        }
        catch (Exception)
        {
            return "?";
        }
    }
}
