using System.Text;
using System.Threading.Channels;
using Microsoft.Extensions.Logging;

sealed class RelayFileLoggerProvider : ILoggerProvider
{
    private readonly RelayFileLogSink _sink;

    public RelayFileLoggerProvider(string directory)
    {
        Directory.CreateDirectory(directory);
        var machine = CleanFilePart(Environment.MachineName);
        var path = Path.Combine(directory, $"deskferry-relay-{machine}-{Environment.ProcessId}.log");
        _sink = new RelayFileLogSink(path);
    }

    public ILogger CreateLogger(string categoryName) => new RelayFileLogger(categoryName, _sink);

    public void Dispose() => _sink.Dispose();

    private static string CleanFilePart(string value)
    {
        foreach (var invalid in Path.GetInvalidFileNameChars())
        {
            value = value.Replace(invalid, '_');
        }
        return value;
    }
}

sealed class RelayFileLogger(string category, RelayFileLogSink sink) : ILogger
{
    public IDisposable? BeginScope<TState>(TState state) where TState : notnull => null;

    public bool IsEnabled(LogLevel logLevel) => logLevel >= LogLevel.Information;

    public void Log<TState>(
        LogLevel logLevel,
        EventId eventId,
        TState state,
        Exception? exception,
        Func<TState, Exception?, string> formatter)
    {
        if (!IsEnabled(logLevel))
        {
            return;
        }

        var message = formatter(state, exception);
        if (string.IsNullOrWhiteSpace(message) && exception is null)
        {
            return;
        }

        sink.Write(logLevel, category, eventId, message, exception);
    }
}

// Writes log lines from a single background task. App Service keeps HOME on
// network storage, so opening the file per line under a lock made every
// logging request thread wait on a remote file operation. Callers now only
// format a line and queue it; the writer keeps the file open and flushes once
// the queue drains, at most about once a second.
sealed class RelayFileLogSink : IDisposable
{
    private const long RotateAtBytes = 8 * 1024 * 1024;
    private const int QueueLimit = 10_000;
    private static readonly TimeSpan FlushInterval = TimeSpan.FromSeconds(1);

    private readonly string _path;
    private readonly Channel<string> _lines = Channel.CreateBounded<string>(new BoundedChannelOptions(QueueLimit)
    {
        // Dropping the newest lines under a flood keeps logging from ever
        // blocking or growing without bound.
        FullMode = BoundedChannelFullMode.DropWrite,
        SingleReader = true,
    });
    private readonly Task _writer;

    public RelayFileLogSink(string path)
    {
        _path = path;
        Write(LogLevel.Information, "DeskFerry.Relay", default, "direct file logging initialized", null);
        _writer = Task.Run(WriteLinesAsync);
    }

    public void Write(LogLevel level, string category, EventId eventId, string message, Exception? exception)
    {
        var line = new StringBuilder(64 + category.Length + message.Length);
        line.Append(DateTimeOffset.UtcNow.ToString("yyyy-MM-ddTHH:mm:ss.fffZ"))
            .Append(' ')
            .Append(level.ToString().ToUpperInvariant())
            .Append(' ')
            .Append(category);
        if (eventId.Id != 0)
        {
            line.Append(" event=").Append(eventId.Id);
        }
        line.Append(' ').Append(message.Replace("\r", " ").Replace("\n", " ")).Append('\n');
        if (exception is not null)
        {
            line.Append(exception.ToString().Replace("\r", " ").Replace("\n", " ")).Append('\n');
        }
        _lines.Writer.TryWrite(line.ToString());
    }

    public void Dispose()
    {
        _lines.Writer.TryComplete();
        try
        {
            _writer.Wait(TimeSpan.FromSeconds(2));
        }
        catch (AggregateException)
        {
        }
    }

    private async Task WriteLinesAsync()
    {
        StreamWriter? writer = null;
        long size = 0;
        var reader = _lines.Reader;
        try
        {
            while (await reader.WaitToReadAsync())
            {
                var flushAt = DateTime.UtcNow + FlushInterval;
                while (reader.TryRead(out var line))
                {
                    try
                    {
                        if (writer is null)
                        {
                            (writer, size) = Open();
                        }
                        else if (size >= RotateAtBytes)
                        {
                            await writer.DisposeAsync();
                            writer = null;
                            Rotate();
                            (writer, size) = Open();
                        }
                        await writer.WriteAsync(line);
                        size += Encoding.UTF8.GetByteCount(line);
                        if (DateTime.UtcNow >= flushAt)
                        {
                            await writer.FlushAsync();
                            flushAt = DateTime.UtcNow + FlushInterval;
                        }
                    }
                    catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
                    {
                        // App Service can transiently remount its persistent
                        // HOME volume; reopen on the next line and never let
                        // logging take down the relay.
                        writer = await CloseQuietlyAsync(writer);
                    }
                }
                try
                {
                    if (writer is not null)
                    {
                        await writer.FlushAsync();
                    }
                }
                catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
                {
                    writer = await CloseQuietlyAsync(writer);
                }
            }
        }
        finally
        {
            await CloseQuietlyAsync(writer);
        }
    }

    private (StreamWriter Writer, long Size) Open()
    {
        var stream = new FileStream(_path, FileMode.Append, FileAccess.Write, FileShare.ReadWrite | FileShare.Delete);
        return (new StreamWriter(stream, new UTF8Encoding(false)), stream.Length);
    }

    private void Rotate()
    {
        var rotated = _path + ".old";
        File.Delete(rotated);
        File.Move(_path, rotated);
    }

    private static async Task<StreamWriter?> CloseQuietlyAsync(StreamWriter? writer)
    {
        if (writer is not null)
        {
            try
            {
                await writer.DisposeAsync();
            }
            catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
            {
            }
        }
        return null;
    }
}
