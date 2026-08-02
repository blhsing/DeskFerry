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

sealed class RelayFileLogSink : IDisposable
{
    private const long RotateAtBytes = 8 * 1024 * 1024;
    private readonly object _gate = new();
    private readonly string _path;
    private bool _disposed;

    public RelayFileLogSink(string path)
    {
        _path = path;
        Write(LogLevel.Information, "DeskFerry.Relay", default, "direct file logging initialized", null);
    }

    public void Write(LogLevel level, string category, EventId eventId, string message, Exception? exception)
    {
        lock (_gate)
        {
            if (_disposed)
            {
                return;
            }

            try
            {
                RotateIfNeeded();
                using var writer = new StreamWriter(new FileStream(_path, FileMode.Append, FileAccess.Write, FileShare.ReadWrite));
                writer.Write(DateTimeOffset.UtcNow.ToString("yyyy-MM-ddTHH:mm:ss.fffZ"));
                writer.Write(' ');
                writer.Write(level.ToString().ToUpperInvariant());
                writer.Write(' ');
                writer.Write(category);
                if (eventId.Id != 0)
                {
                    writer.Write(" event=");
                    writer.Write(eventId.Id);
                }
                writer.Write(' ');
                writer.WriteLine(message.Replace("\r", " ").Replace("\n", " "));
                if (exception is not null)
                {
                    writer.WriteLine(exception.ToString().Replace("\r", " ").Replace("\n", " "));
                }
            }
            catch (IOException)
            {
                // Logging must never take down the relay when App Service storage is unavailable.
            }
            catch (UnauthorizedAccessException)
            {
                // App Service can transiently remount its persistent HOME volume.
            }
        }
    }

    public void Dispose()
    {
        lock (_gate)
        {
            _disposed = true;
        }
    }

    private void RotateIfNeeded()
    {
        var file = new FileInfo(_path);
        if (!file.Exists || file.Length < RotateAtBytes)
        {
            return;
        }

        var rotated = _path + ".old";
        File.Delete(rotated);
        File.Move(_path, rotated);
    }
}
