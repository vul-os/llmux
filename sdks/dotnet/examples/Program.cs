using System;
using System.Threading.Tasks;

namespace Llmux.Examples
{
    /// <summary>
    /// Entry point for the two llmux examples.
    ///
    ///   dotnet run --project sdks/dotnet/examples -- direct
    ///   dotnet run --project sdks/dotnet/examples -- sidecar
    ///
    /// Prefer sdks/dotnet/run-examples.sh, which builds the shared library and
    /// the gateway binary and stands up a fake upstream first.
    /// </summary>
    internal static class Program
    {
        internal static async Task<int> Main(string[] args)
        {
            string which = args.Length > 0 ? args[0] : "both";
            int status = 0;

            if (which is "both" or "direct")
            {
                Console.WriteLine("================ DirectChat (in-process, C ABI) ================");
                status |= await DirectChat.RunAsync();
                Console.WriteLine();
            }

            if (which is "both" or "sidecar")
            {
                Console.WriteLine("================ SidecarChat (child process, HTTP) =============");
                status |= await SidecarChat.RunAsync();
                Console.WriteLine();
            }

            if (which is not ("both" or "direct" or "sidecar"))
            {
                Console.Error.WriteLine($"unknown example: {which} (want: direct, sidecar, both)");
                return 2;
            }

            return status;
        }
    }
}
