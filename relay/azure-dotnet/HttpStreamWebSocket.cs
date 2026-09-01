using System.Buffers.Binary;
using System.Collections.Concurrent;
using System.Net.WebSockets;
using System.Security.Cryptography;
using System.Threading.Channels;

sealed class HttpStreamRegistry : IDisposable
{
    private static readonly TimeSpan Retention = TimeSpan.FromMinutes(5);
    private readonly ConcurrentDictionary<string, HttpStreamWebSocket> _streams = new(StringComparer.Ordinal);
    private readonly Timer _sweeper;

    public HttpStreamRegistry()
    {
        _sweeper = new Timer(_ => Sweep(), null, TimeSpan.FromMinutes(1), TimeSpan.FromMinutes(1));
    }

    public (HttpStreamWebSocket Socket, bool Created) GetOrCreate(string room, string id, string secret)
    {
        var key = $"{room.ToLowerInvariant()}/{id}";
		if (!_streams.ContainsKey(key) && _streams.Count >= 4096)
		{
			throw new InvalidOperationException("HTTP stream capacity reached.");
		}
        var candidate = new HttpStreamWebSocket(secret);
        var socket = _streams.GetOrAdd(key, candidate);
        if (!ReferenceEquals(socket, candidate))
        {
            candidate.Abort();
        }
        return (socket, ReferenceEquals(socket, candidate));
    }

    private void Sweep()
    {
        var cutoff = DateTimeOffset.UtcNow - Retention;
        foreach (var entry in _streams)
        {
            if ((entry.Value.IsTerminal || entry.Value.LastActivity < cutoff) &&
                _streams.TryRemove(entry.Key, out var removed) && ReferenceEquals(removed, entry.Value))
            {
                entry.Value.Abort();
            }
        }
    }

    public void Dispose()
    {
        _sweeper.Dispose();
        foreach (var stream in _streams.Values)
        {
            stream.Abort();
        }
        _streams.Clear();
    }
}

sealed class HttpStreamWebSocket : WebSocket
{
    private const byte AckRecord = 0;
    private const byte TextRecord = 1;
    private const byte BinaryRecord = 2;
    private const byte CloseRecord = 8;
    private const int HeaderLength = 13;
    private const int ReadLimit = 1 << 20;
    private const int MaxBuffered = 8 * 1024 * 1024;
    private static readonly TimeSpan Keepalive = TimeSpan.FromSeconds(10);

    private sealed record Frame(byte Kind, ulong Sequence, byte[] Payload);

    private readonly object _gate = new();
    private readonly string _secret;
    private readonly CancellationTokenSource _lifetime = new();
    private readonly Channel<Frame> _received = Channel.CreateUnbounded<Frame>(new UnboundedChannelOptions { SingleReader = false, SingleWriter = false });
    private readonly SemaphoreSlim _changed = new(0, 1);
    private readonly SemaphoreSlim _sendLock = new(1, 1);
    private readonly List<Frame> _outbound = [];
    private ulong _nextSend = 1;
    private ulong _nextReceive = 1;
    private int _buffered;
    private long _upGeneration;
    private long _downGeneration;
    private bool _downBatch;
    private bool _downPrimed;
    private WebSocketState _state = WebSocketState.Open;
    private WebSocketCloseStatus? _closeStatus;
    private string? _closeDescription;
    private Frame? _currentReceive;
    private int _currentReceiveOffset;
    private MemoryStream? _fragmentedSend;
    private WebSocketMessageType _fragmentedType;

    public HttpStreamWebSocket(string secret)
    {
        _secret = secret;
        LastActivity = DateTimeOffset.UtcNow;
    }

    public CancellationToken Lifetime => _lifetime.Token;
    public DateTimeOffset LastActivity { get; private set; }
    public bool IsTerminal => _lifetime.IsCancellationRequested;
    public bool SecretMatches(string value) => CryptographicOperations.FixedTimeEquals(
        System.Text.Encoding.UTF8.GetBytes(_secret), System.Text.Encoding.UTF8.GetBytes(value));

    public void EnableBatchDownloads()
    {
        lock (_gate)
        {
            _downBatch = true;
        }
    }

    public override WebSocketCloseStatus? CloseStatus => _closeStatus;
    public override string? CloseStatusDescription => _closeDescription;
    public override string? SubProtocol => null;
    public override WebSocketState State => _state;

    public override void Abort()
    {
        lock (_gate)
        {
            if (_state == WebSocketState.Aborted)
            {
                return;
            }
            _state = WebSocketState.Aborted;
        }
        _lifetime.Cancel();
        _received.Writer.TryComplete(new WebSocketException(WebSocketError.ConnectionClosedPrematurely));
        SignalChanged();
    }

    public override Task CloseAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken) =>
        CloseOutputAsync(closeStatus, statusDescription, cancellationToken);

    public override async Task CloseOutputAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken)
    {
        byte[] reason = System.Text.Encoding.UTF8.GetBytes(statusDescription ?? "");
        if (reason.Length > 123)
        {
            reason = reason[..123];
        }
        var payload = new byte[2 + reason.Length];
        BinaryPrimitives.WriteUInt16BigEndian(payload, (ushort)closeStatus);
        reason.CopyTo(payload.AsSpan(2));
        await QueueOutboundAsync(CloseRecord, payload, cancellationToken);
        lock (_gate)
        {
            _closeStatus = closeStatus;
            _closeDescription = statusDescription;
            _state = WebSocketState.CloseSent;
        }
        _ = Task.Run(async () =>
        {
            await Task.Delay(TimeSpan.FromSeconds(1));
            _lifetime.Cancel();
            _received.Writer.TryComplete();
            lock (_gate)
            {
                if (_state != WebSocketState.Aborted)
                {
                    _state = WebSocketState.Closed;
                }
            }
        });
    }

    public override async Task<WebSocketReceiveResult> ReceiveAsync(ArraySegment<byte> buffer, CancellationToken cancellationToken)
    {
        while (true)
        {
            Frame frame;
            lock (_gate)
            {
                if (_currentReceive is not null)
                {
                    frame = _currentReceive;
                    goto CopyFrame;
                }
            }

            try
            {
                frame = await _received.Reader.ReadAsync(cancellationToken);
            }
            catch (ChannelClosedException exception)
            {
                throw new WebSocketException(WebSocketError.ConnectionClosedPrematurely, exception);
            }
            lock (_gate)
            {
                _currentReceive = frame;
                _currentReceiveOffset = 0;
            }

        CopyFrame:
            if (frame.Kind == CloseRecord)
            {
                var status = frame.Payload.Length >= 2
                    ? (WebSocketCloseStatus)BinaryPrimitives.ReadUInt16BigEndian(frame.Payload)
                    : WebSocketCloseStatus.NormalClosure;
                var reason = frame.Payload.Length > 2 ? System.Text.Encoding.UTF8.GetString(frame.Payload, 2, frame.Payload.Length - 2) : "";
                lock (_gate)
                {
                    _currentReceive = null;
                    _closeStatus = status;
                    _closeDescription = reason;
                    _state = WebSocketState.CloseReceived;
                }
                return new WebSocketReceiveResult(0, WebSocketMessageType.Close, true, status, reason);
            }

            int copied;
            bool complete;
            lock (_gate)
            {
                copied = Math.Min(buffer.Count, frame.Payload.Length - _currentReceiveOffset);
                frame.Payload.AsSpan(_currentReceiveOffset, copied).CopyTo(buffer.AsSpan());
                _currentReceiveOffset += copied;
                complete = _currentReceiveOffset == frame.Payload.Length;
                if (complete)
                {
                    _currentReceive = null;
                    _currentReceiveOffset = 0;
                }
            }
            return new WebSocketReceiveResult(copied, frame.Kind == TextRecord ? WebSocketMessageType.Text : WebSocketMessageType.Binary, complete);
        }
    }

    public override async Task SendAsync(ArraySegment<byte> buffer, WebSocketMessageType messageType, bool endOfMessage, CancellationToken cancellationToken)
    {
        if (messageType is not (WebSocketMessageType.Text or WebSocketMessageType.Binary))
        {
            throw new WebSocketException(WebSocketError.InvalidMessageType);
        }
        await _sendLock.WaitAsync(cancellationToken);
        try
        {
            if (_fragmentedSend is null && endOfMessage)
            {
                await QueueOutboundAsync(messageType == WebSocketMessageType.Text ? TextRecord : BinaryRecord, buffer.ToArray(), cancellationToken);
                return;
            }
            _fragmentedSend ??= new MemoryStream();
            if (_fragmentedSend.Length == 0)
            {
                _fragmentedType = messageType;
            }
            else if (_fragmentedType != messageType)
            {
                throw new WebSocketException(WebSocketError.InvalidMessageType);
            }
            await _fragmentedSend.WriteAsync(buffer.AsMemory(), cancellationToken);
            if (endOfMessage)
            {
                var payload = _fragmentedSend.ToArray();
                _fragmentedSend.Dispose();
                _fragmentedSend = null;
                await QueueOutboundAsync(messageType == WebSocketMessageType.Text ? TextRecord : BinaryRecord, payload, cancellationToken);
            }
        }
        finally
        {
            _sendLock.Release();
        }
    }

    private async Task QueueOutboundAsync(byte kind, byte[] payload, CancellationToken cancellationToken)
    {
        if (payload.Length > ReadLimit)
        {
            throw new WebSocketException(WebSocketError.HeaderError, "HTTP stream message exceeds the relay limit.");
        }
        while (true)
        {
            cancellationToken.ThrowIfCancellationRequested();
            lock (_gate)
            {
                if (_state is WebSocketState.Aborted or WebSocketState.Closed)
                {
                    throw new WebSocketException(WebSocketError.ConnectionClosedPrematurely);
                }
                if (_buffered + payload.Length <= MaxBuffered)
                {
                    _outbound.Add(new Frame(kind, _nextSend++, payload));
                    _buffered += payload.Length;
                    SignalChanged();
                    return;
                }
            }
            await Task.Delay(25, cancellationToken);
        }
    }

    public async Task ServeUploadAsync(HttpContext context)
    {
        if (!HttpMethods.IsPost(context.Request.Method))
        {
            context.Response.StatusCode = StatusCodes.Status405MethodNotAllowed;
            return;
        }
        var generation = Interlocked.Increment(ref _upGeneration);
        try
        {
            while (!context.RequestAborted.IsCancellationRequested && generation == Volatile.Read(ref _upGeneration))
            {
                var frame = await ReadRecordAsync(context.Request.Body, context.RequestAborted);
                ApplyRecord(frame);
            }
        }
        catch (Exception exception) when (exception is EndOfStreamException or IOException or OperationCanceledException) { }
        context.Response.StatusCode = StatusCodes.Status204NoContent;
    }

    public async Task ServeDownloadAsync(HttpContext context)
    {
        if (!HttpMethods.IsGet(context.Request.Method))
        {
            context.Response.StatusCode = StatusCodes.Status405MethodNotAllowed;
            return;
        }
        var generation = Interlocked.Increment(ref _downGeneration);
        bool batchMode;
        bool primeBatch;
        lock (_gate)
        {
            batchMode = _downBatch;
            primeBatch = batchMode && !_downPrimed;
            if (primeBatch)
            {
                _downPrimed = true;
            }
        }
        if (batchMode)
        {
            await ServeDownloadBatchAsync(context, generation, primeBatch);
            return;
        }
        context.Response.StatusCode = StatusCodes.Status200OK;
        context.Response.ContentType = "application/octet-stream";
        context.Response.Headers.CacheControl = "no-store, no-transform";
        context.Response.Headers["X-Accel-Buffering"] = "no";
        await context.Response.StartAsync(context.RequestAborted);

        ulong lastSequence = 0;
        ulong lastAck = 0;
        bool forceAck = true;
        try
        {
            while (!context.RequestAborted.IsCancellationRequested && !_lifetime.IsCancellationRequested && generation == Volatile.Read(ref _downGeneration))
            {
                List<Frame> frames;
                ulong ack;
                lock (_gate)
                {
                    frames = _outbound.Where(frame => frame.Sequence > lastSequence).ToList();
                    ack = _nextReceive - 1;
                }
                if (forceAck || ack > lastAck)
                {
                    await WriteRecordAsync(context.Response.Body, new Frame(AckRecord, ack, []), context.RequestAborted);
                    lastAck = ack;
                    forceAck = false;
                }
                foreach (var frame in frames)
                {
                    await WriteRecordAsync(context.Response.Body, frame, context.RequestAborted);
                    lastSequence = frame.Sequence;
                }
                await context.Response.Body.FlushAsync(context.RequestAborted);

                using var timeout = new CancellationTokenSource(Keepalive);
                using var linked = CancellationTokenSource.CreateLinkedTokenSource(context.RequestAborted, _lifetime.Token, timeout.Token);
                try
                {
                    await _changed.WaitAsync(linked.Token);
                }
                catch (OperationCanceledException) when (timeout.IsCancellationRequested && !context.RequestAborted.IsCancellationRequested && !_lifetime.IsCancellationRequested)
                {
                    forceAck = true;
                }
            }
        }
        catch (Exception exception) when (exception is IOException or OperationCanceledException) { }
    }

    private async Task ServeDownloadBatchAsync(HttpContext context, long generation, bool prime)
    {
        while (!context.RequestAborted.IsCancellationRequested && !_lifetime.IsCancellationRequested && generation == Volatile.Read(ref _downGeneration))
        {
            List<Frame> frames;
            ulong ack;
            lock (_gate)
            {
                frames = _outbound.ToList();
                ack = _nextReceive - 1;
            }
            if (prime || frames.Count > 0)
            {
                await WriteDownloadBatchAsync(context, ack, frames);
                return;
            }

            using var timeout = new CancellationTokenSource(Keepalive);
            using var linked = CancellationTokenSource.CreateLinkedTokenSource(context.RequestAborted, _lifetime.Token, timeout.Token);
            try
            {
                await _changed.WaitAsync(linked.Token);
            }
            catch (OperationCanceledException) when (timeout.IsCancellationRequested && !context.RequestAborted.IsCancellationRequested && !_lifetime.IsCancellationRequested)
            {
                await WriteDownloadBatchAsync(context, ack, []);
                return;
            }
        }
    }

    private static async Task WriteDownloadBatchAsync(HttpContext context, ulong ack, List<Frame> frames)
    {
        await using var payload = new MemoryStream();
        await WriteRecordAsync(payload, new Frame(AckRecord, ack, []), context.RequestAborted);
        foreach (var frame in frames)
        {
            await WriteRecordAsync(payload, frame, context.RequestAborted);
        }
        context.Response.StatusCode = StatusCodes.Status200OK;
        context.Response.ContentType = "application/octet-stream";
        context.Response.ContentLength = payload.Length;
        context.Response.Headers.CacheControl = "no-store, no-transform";
        payload.Position = 0;
        await payload.CopyToAsync(context.Response.Body, context.RequestAborted);
    }

    private void ApplyRecord(Frame frame)
    {
        lock (_gate)
        {
            LastActivity = DateTimeOffset.UtcNow;
            if (frame.Kind == AckRecord)
            {
                if (frame.Sequence >= _nextSend)
                {
                    throw new InvalidDataException("HTTP stream acknowledgement exceeds the sent sequence.");
                }
                while (_outbound.Count > 0 && _outbound[0].Sequence <= frame.Sequence)
                {
                    _buffered -= _outbound[0].Payload.Length;
                    _outbound.RemoveAt(0);
                }
                return;
            }
            if (frame.Sequence < _nextReceive)
            {
                SignalChanged();
                return;
            }
            if (frame.Sequence != _nextReceive || frame.Kind is not (TextRecord or BinaryRecord or CloseRecord))
            {
                throw new InvalidDataException("HTTP stream sequence or record type is invalid.");
            }
            _nextReceive++;
            _received.Writer.TryWrite(frame);
            SignalChanged();
        }
    }

    private void SignalChanged()
    {
        try { _changed.Release(); } catch (SemaphoreFullException) { }
    }

    private static async Task<Frame> ReadRecordAsync(Stream stream, CancellationToken cancellationToken)
    {
        var header = new byte[HeaderLength];
        await stream.ReadExactlyAsync(header, cancellationToken);
        var length = BinaryPrimitives.ReadUInt32BigEndian(header.AsSpan(9));
        if (length > ReadLimit)
        {
            throw new InvalidDataException("HTTP stream record exceeds the relay limit.");
        }
        var payload = new byte[(int)length];
        await stream.ReadExactlyAsync(payload, cancellationToken);
        return new Frame(header[0], BinaryPrimitives.ReadUInt64BigEndian(header.AsSpan(1)), payload);
    }

    private static async Task WriteRecordAsync(Stream stream, Frame frame, CancellationToken cancellationToken)
    {
        var header = new byte[HeaderLength];
        header[0] = frame.Kind;
        BinaryPrimitives.WriteUInt64BigEndian(header.AsSpan(1), frame.Sequence);
        BinaryPrimitives.WriteUInt32BigEndian(header.AsSpan(9), (uint)frame.Payload.Length);
        await stream.WriteAsync(header, cancellationToken);
        if (frame.Payload.Length > 0)
        {
            await stream.WriteAsync(frame.Payload, cancellationToken);
        }
    }

    public override void Dispose()
    {
        Abort();
        _lifetime.Dispose();
        _changed.Dispose();
        _sendLock.Dispose();
        _fragmentedSend?.Dispose();
    }
}
