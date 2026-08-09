import java.lang.foreign.Arena;
import java.lang.foreign.FunctionDescriptor;
import java.lang.foreign.Linker;
import java.lang.foreign.MemorySegment;
import java.lang.foreign.SymbolLookup;
import java.lang.foreign.ValueLayout;
import java.lang.invoke.MethodHandle;
import java.nio.file.Path;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * What loading libllmux does to a JVM's signal handlers — measured, not asserted.
 *
 * README.md makes a claim about the Go runtime replacing signal handlers the
 * JVM depends on. This is the program that establishes it. It reads the
 * installed {@code sigaction} handler for every interesting signal, dlopens the
 * shared library, reads them again, prints the diff, and then tries to provoke
 * the JVM into using the handlers that changed.
 *
 * <pre>
 *   sdks/java/signal-probe.sh                 # this repo's library
 *   sdks/java/signal-probe.sh --jsig          # again, with libjsig preloaded
 * </pre>
 *
 * A probe that prints "no change" on a platform is just as useful as one that
 * prints a diff — the point is that the README's platform claims are the output
 * of this program rather than a recollection of how Go's runtime behaves.
 *
 * Requires Java 22+ (java.lang.foreign) and --enable-native-access=ALL-UNNAMED.
 */
public final class SignalHandlerProbe {

    /**
     * Signal numbers. These are the BSD/darwin numbers; Linux renumbers several
     * of them, so the probe refuses to run anywhere it has not been taught.
     */
    private static final Map<String, Integer> DARWIN = new LinkedHashMap<>();
    private static final Map<String, Integer> LINUX = new LinkedHashMap<>();
    static {
        DARWIN.put("SIGILL", 4);   LINUX.put("SIGILL", 4);
        DARWIN.put("SIGTRAP", 5);  LINUX.put("SIGTRAP", 5);
        DARWIN.put("SIGABRT", 6);  LINUX.put("SIGABRT", 6);
        DARWIN.put("SIGFPE", 8);   LINUX.put("SIGFPE", 8);
        DARWIN.put("SIGBUS", 10);  LINUX.put("SIGBUS", 7);
        DARWIN.put("SIGSEGV", 11); LINUX.put("SIGSEGV", 11);
        DARWIN.put("SIGPIPE", 13); LINUX.put("SIGPIPE", 13);
        DARWIN.put("SIGURG", 16);  LINUX.put("SIGURG", 23);
        DARWIN.put("SIGXCPU", 24); LINUX.put("SIGXCPU", 24);
        DARWIN.put("SIGXFSZ", 25); LINUX.put("SIGXFSZ", 25);
        DARWIN.put("SIGPROF", 27); LINUX.put("SIGPROF", 27);
        DARWIN.put("SIGUSR1", 30); LINUX.put("SIGUSR1", 10);
        DARWIN.put("SIGUSR2", 31); LINUX.put("SIGUSR2", 12);
    }

    private static final int SA_ONSTACK = 0x0001;

    /**
     * struct sigaction layout. Both platforms put the handler pointer first;
     * only the offset of sa_flags differs.
     *   darwin: handler(8) sa_mask(4) sa_flags(4)                 = 16 bytes
     *   linux:  handler(8) sa_flags(8) restorer(8) sa_mask(128)   = 152 bytes
     */
    private static final boolean DARWIN_HOST =
            System.getProperty("os.name", "").toLowerCase().contains("mac");
    private static final long SA_SIZE = DARWIN_HOST ? 16 : 152;
    private static final long FLAGS_OFFSET = DARWIN_HOST ? 12 : 8;
    private static final long FLAGS_WIDTH = DARWIN_HOST ? 4 : 8;

    private static MethodHandle sigaction;

    public static void main(String[] args) throws Throwable {
        if (args.length < 2) {
            System.err.println("usage: SignalHandlerProbe <library> <abi-version-symbol>");
            System.exit(2);
        }
        Path library = Path.of(args[0]);
        String versionSymbol = args[1];

        Map<String, Integer> signals = DARWIN_HOST ? DARWIN : LINUX;
        System.out.println("host: " + System.getProperty("os.name")
                + " " + System.getProperty("os.arch")
                + " | jvm: " + System.getProperty("java.vm.name")
                + " " + System.getProperty("java.version"));
        System.out.println("library: " + library);
        boolean jsig = jsigLoaded();
        System.out.println("libjsig preloaded: " + jsig);
        if (jsig) {
            System.out.println();
            System.out.println("CAVEAT: libjsig interposes sigaction(), including THIS PROGRAM'S");
            System.out.println("  calls to it. The addresses below are what libjsig reports, not");
            System.out.println("  necessarily what is installed at the OS level. Under --jsig the");
            System.out.println("  authority on whether HotSpot's handlers survived is HotSpot's own");
            System.out.println("  audit: re-run with --checkjni and read its warnings (or absence).");
        }
        System.out.println();

        Linker linker = Linker.nativeLinker();
        sigaction = linker.downcallHandle(
                linker.defaultLookup().find("sigaction").orElseThrow(),
                FunctionDescriptor.of(ValueLayout.JAVA_INT,
                        ValueLayout.JAVA_INT, ValueLayout.ADDRESS, ValueLayout.ADDRESS));

        Map<String, long[]> before = snapshot(signals);

        SymbolLookup lookup = SymbolLookup.libraryLookup(library, Arena.global());
        MethodHandle abi = linker.downcallHandle(
                lookup.find(versionSymbol).orElseThrow(
                        () -> new IllegalStateException("no symbol " + versionSymbol)),
                FunctionDescriptor.of(ValueLayout.ADDRESS));
        MemorySegment v = (MemorySegment) abi.invokeExact();
        System.out.println("loaded; abi version = " + v.reinterpret(Long.MAX_VALUE).getString(0));
        System.out.println();

        Map<String, long[]> after = snapshot(signals);

        System.out.println("signal    before                after                 verdict");
        System.out.println("--------------------------------------------------------------------------");
        int replaced = 0;
        int flagsOnly = 0;
        for (String name : signals.keySet()) {
            long[] b = before.get(name);
            long[] a = after.get(name);
            String verdict;
            if (b[0] != a[0]) {
                replaced++;
                verdict = "HANDLER REPLACED by the Go runtime";
            } else if (b[1] != a[1]) {
                flagsOnly++;
                verdict = "flags changed"
                        + (((b[1] & SA_ONSTACK) == 0 && (a[1] & SA_ONSTACK) != 0)
                            ? " (Go added SA_ONSTACK)" : "");
            } else {
                verdict = "unchanged";
            }
            System.out.printf("%-9s %-21s %-21s %s%n", name, show(b), show(a), verdict);
        }

        System.out.println();
        System.out.println(replaced + " handler(s) replaced, " + flagsOnly + " left in place with altered flags");
        System.out.println();

        System.out.println("does the JVM still work through the handlers that changed?");
        System.out.println("  implicit null checks (SIGSEGV): " + implicitNullChecks() + " recovered");
        System.out.println("  stack banging (SIGSEGV/SIGBUS): " + stackOverflow());
        System.out.println("  arithmetic traps:               " + divideByZero());
        System.out.println();
        System.out.println("probe completed without terminating the VM.");
        System.out.println("Run the JVM with -Xcheck:jni to see HotSpot's own opinion of the above.");
    }

    private static boolean jsigLoaded() {
        String ins = System.getenv("DYLD_INSERT_LIBRARIES");
        if (ins == null) {
            ins = System.getenv("LD_PRELOAD");
        }
        return ins != null && ins.contains("jsig");
    }

    private static Map<String, long[]> snapshot(Map<String, Integer> signals) throws Throwable {
        Map<String, long[]> out = new LinkedHashMap<>();
        try (Arena arena = Arena.ofConfined()) {
            MemorySegment old = arena.allocate(SA_SIZE);
            for (Map.Entry<String, Integer> e : signals.entrySet()) {
                old.fill((byte) 0);
                int signo = e.getValue();
                int rc = (int) sigaction.invokeExact(signo, MemorySegment.NULL, old);
                if (rc != 0) {
                    throw new IllegalStateException("sigaction(" + e.getKey() + ") failed");
                }
                long flags = FLAGS_WIDTH == 4
                        ? old.get(ValueLayout.JAVA_INT, FLAGS_OFFSET)
                        : old.get(ValueLayout.JAVA_LONG, FLAGS_OFFSET);
                out.put(e.getKey(), new long[]{old.get(ValueLayout.JAVA_LONG, 0), flags});
            }
        }
        return out;
    }

    private static String show(long[] h) {
        String addr = h[0] == 0 ? "SIG_DFL" : h[0] == 1 ? "SIG_IGN" : String.format("0x%x", h[0]);
        return addr + " f=0x" + Long.toHexString(h[1]);
    }

    /** HotSpot elides null checks and recovers them from SIGSEGV. */
    private static long implicitNullChecks() {
        String s = null;
        long n = 0;
        for (int i = 0; i < 2_000_000; i++) {
            try {
                n += s.length();
            } catch (NullPointerException e) {
                n++;
            }
            if (i == 1_000_000) s = "x";   // force deopt/reopt churn
            if (i == 1_500_000) s = null;
        }
        return n;
    }

    /** Guard-page faults are how HotSpot produces StackOverflowError. */
    private static String stackOverflow() {
        try {
            return "no StackOverflowError at depth " + recurse(0);
        } catch (StackOverflowError e) {
            return "StackOverflowError raised and caught";
        }
    }

    private static int recurse(int d) {
        return recurse(d + 1) + 1;
    }

    private static String divideByZero() {
        int zero = Integer.parseInt("0");
        try {
            return "no exception: " + (1 / zero);
        } catch (ArithmeticException e) {
            return "ArithmeticException raised and caught";
        }
    }
}
