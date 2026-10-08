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
    private const int ReorderLimit = 4096;
    private static readonly TimeSpan Keepalive = TimeSpan.FromSeconds(10);
    private static readonly TimeSpan AckCoalesce = TimeSpan.FromMilliseconds(20);
    private static readonly TimeSpan ResendAfter = TimeSpan.FromSeconds(3);

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
    // Receive acknowledgement carried by the last downstream batch, so a parked
    // batch GET can answer as soon as it advances.
    private ulong _downAckSent;
    // Records that arrived before an earlier sequence, as pipelined uploads
    // can complete out of order.
    private readonly Dictionary<ulong, Frame> _receiveAhead = [];
    private int _receiveAheadBytes;
    // Pipelined downloads: the highest sequence a response has carried, and
    // when unacknowledged dispatched records last made progress.
    private ulong _downDispatched;
    private DateTimeOffset? _downStallSince;
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

    public override Task CloseOutputAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken) =>
        CloseOutputWithGraceAsync(closeStatus, statusDescription, cancellationToken, TimeSpan.FromSeconds(1));

    private async Task CloseOutputWithGraceAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken, TimeSpan grace)
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
            await Task.Delay(grace);
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
        // Pipelined uploads overlap by design, and out-of-order records are
        // reordered on receipt. Only a legacy upload is superseded by a newer one.
        var pipelined = !string.IsNullOrEmpty(context.Request.Headers["X-DeskFerry-Stream-Pipeline"].FirstOrDefault());
        if (pipelined)
        {
            context.Response.Headers["X-DeskFerry-Stream-Pipeline"] = "1";
        }
        var generation = Interlocked.Increment(ref _upGeneration);
        try
        {
            while (!context.RequestAborted.IsCancellationRequested && (pipelined || generation == Volatile.Read(ref _upGeneration)))
            {
                var frame = await ReadRecordAsync(context.Request.Body, context.RequestAborted);
                // A relay restart can recreate an existing transport ID with
                // fresh counters. Older clients keep retrying the old upload
                // and ignore our lower downstream sequences. Send the close at
                // the sequence they expect, based on their authenticated ACK.
                if (frame.Kind == AckRecord && ResetLostSequence(frame.Sequence))
                {
                    await CloseOutputWithGraceAsync(WebSocketCloseStatus.EndpointUnavailable, "HTTP stream state lost; reconnect", context.RequestAborted, TimeSpan.FromSeconds(30));
                    context.Response.StatusCode = StatusCodes.Status409Conflict;
                    return;
                }
                ApplyRecord(frame);
            }
        }
        catch (Exception exception) when (exception is EndOfStreamException or IOException or OperationCanceledException) { }
        context.Response.StatusCode = StatusCodes.Status204NoContent;
    }

    private bool ResetLostSequence(ulong acknowledged)
    {
        lock (_gate)
        {
            if (acknowledged < _nextSend) return false;
            if (acknowledged == ulong.MaxValue) throw new InvalidDataException("HTTP stream acknowledgement overflow.");
            _outbound.Clear();
            _buffered = 0;
            _nextSend = acknowledged + 1;
            return true;
        }
    }

    public async Task ServeDownloadAsync(HttpContext context)
    {
        if (!HttpMethods.IsGet(context.Request.Method))
        {
            context.Response.StatusCode = StatusCodes.Status405MethodNotAllowed;
            return;
        }
        // Clients report their received sequence on each GET. Applying it here
        // keeps batches free of records the client already has and spares it
        // ack-only POSTs; echoing the header advertises that support.
        var ackHeader = context.Request.Headers["X-DeskFerry-Stream-Ack"].FirstOrDefault()?.Trim();
        if (ulong.TryParse(ackHeader, out var downstreamAck))
        {
            ApplyDownstreamAck(downstreamAck);
            context.Response.Headers["X-DeskFerry-Stream-Ack"] = ackHeader;
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
            await ServeDownloadBatchAsync(context, generation, primeBatch, !string.IsNullOrEmpty(context.Request.Headers["X-DeskFerry-Stream-Pipeline"].FirstOrDefault()));
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

    private async Task ServeDownloadBatchAsync(HttpContext context, long generation, bool prime, bool pipelined)
    {
        if (pipelined)
        {
            context.Response.Headers["X-DeskFerry-Stream-Pipeline"] = "1";
        }
        var keepaliveDeadline = DateTimeOffset.UtcNow + Keepalive;
        DateTimeOffset? coalesceDeadline = null;
        using var linked = CancellationTokenSource.CreateLinkedTokenSource(context.RequestAborted, _lifetime.Token);
        // A newer GET supersedes a legacy one; pipelined GETs wait together.
        while (!context.RequestAborted.IsCancellationRequested && !_lifetime.IsCancellationRequested && (pipelined || generation == Volatile.Read(ref _downGeneration)))
        {
            List<Frame>? frames;
            ulong ack;
            bool ackAdvanced;
            DateTimeOffset? resendAt;
            lock (_gate)
            {
                frames = ClaimDownload(pipelined, prime, out ack);
                ackAdvanced = ack > _downAckSent;
                resendAt = DownloadResendAt(pipelined);
            }
            if (frames is not null)
            {
                await WriteDownloadBatchAsync(context, ack, frames);
                return;
            }
            // Answer an advanced acknowledgement promptly so the sender can free
            // its buffer, but give the reply that usually follows a received
            // message a moment to share this batch instead of the next poll.
            if (ackAdvanced && coalesceDeadline is null)
            {
                coalesceDeadline = DateTimeOffset.UtcNow + AckCoalesce;
            }
            var deadline = keepaliveDeadline;
            if (coalesceDeadline is { } coalesce && coalesce < deadline)
            {
                deadline = coalesce;
            }
            if (resendAt is { } resend && resend < deadline)
            {
                deadline = resend;
            }
            var wait = deadline - DateTimeOffset.UtcNow;
            if (wait > TimeSpan.Zero && await _changed.WaitAsync(wait, linked.Token))
            {
                continue;
            }
            var now = DateTimeOffset.UtcNow;
            var keepaliveDue = now >= keepaliveDeadline;
            var coalesceDue = coalesceDeadline is { } due && now >= due;
            lock (_gate)
            {
                frames = ClaimDownload(pipelined, keepaliveDue || (coalesceDue && _nextReceive - 1 > _downAckSent), out ack);
            }
            if (frames is not null)
            {
                await WriteDownloadBatchAsync(context, ack, frames);
                return;
            }
            if (coalesceDue)
            {
                // Another waiting GET already carried the acknowledgement.
                coalesceDeadline = null;
            }
        }
    }

    // Chooses the records for a downstream batch, or returns null to keep
    // waiting. A legacy GET carries every unacknowledged record. Pipelined GETs
    // each carry only records no other response has carried, so concurrently
    // waiting GETs never duplicate data, unless dispatched records stayed
    // unacknowledged long enough that their response was probably lost.
    // Callers hold _gate.
    private List<Frame>? ClaimDownload(bool pipelined, bool force, out ulong ack)
    {
        ack = _nextReceive - 1;
        ulong after = 0;
        if (pipelined)
        {
            if (DownloadResendAt(true) is { } resendAt && resendAt <= DateTimeOffset.UtcNow)
            {
                _downDispatched = _outbound[0].Sequence - 1;
                _downStallSince = null;
            }
            after = _downDispatched;
        }
        var frames = _outbound.Where(frame => frame.Sequence > after).ToList();
        if (frames.Count == 0 && !force)
        {
            return null;
        }
        if (pipelined && frames.Count > 0)
        {
            if (!DownloadOutstanding())
            {
                _downStallSince = DateTimeOffset.UtcNow;
            }
            _downDispatched = frames[^1].Sequence;
        }
        if (ack > _downAckSent)
        {
            _downAckSent = ack;
        }
        return frames;
    }

    private bool DownloadOutstanding() => _outbound.Count > 0 && _outbound[0].Sequence <= _downDispatched;

    // When outstanding pipelined records count as lost. Callers hold _gate.
    private DateTimeOffset? DownloadResendAt(bool pipelined) =>
        pipelined && DownloadOutstanding() && _downStallSince is { } since ? since + ResendAfter : null;

    // Restarts the lost-response clock after acknowledgements trim the send
    // queue. Callers hold _gate.
    private void NoteSendProgress()
    {
        _downStallSince = DownloadOutstanding() ? DateTimeOffset.UtcNow : null;
    }

    private void ApplyDownstreamAck(ulong acknowledged)
    {
        lock (_gate)
        {
            // Lost-state recovery for acknowledgements beyond the sent sequence
            // belongs to the upload path.
            if (acknowledged >= _nextSend)
            {
                return;
            }
            LastActivity = DateTimeOffset.UtcNow;
            var trimmed = false;
            while (_outbound.Count > 0 && _outbound[0].Sequence <= acknowledged)
            {
                _buffered -= _outbound[0].Payload.Length;
                _outbound.RemoveAt(0);
                trimmed = true;
            }
            if (trimmed)
            {
                NoteSendProgress();
            }
        }
    }

    private async Task WriteDownloadBatchAsync(HttpContext context, ulong ack, List<Frame> frames)
    {
        lock (_gate)
        {
            if (ack > _downAckSent)
            {
                _downAckSent = ack;
            }
        }
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
                var trimmed = false;
                while (_outbound.Count > 0 && _outbound[0].Sequence <= frame.Sequence)
                {
                    _buffered -= _outbound[0].Payload.Length;
                    _outbound.RemoveAt(0);
                    trimmed = true;
                }
                if (trimmed)
                {
                    NoteSendProgress();
                }
                return;
            }
            if (frame.Sequence < _nextReceive)
            {
                SignalChanged();
                return;
            }
            if (frame.Kind is not (TextRecord or BinaryRecord or CloseRecord))
            {
                throw new InvalidDataException("HTTP stream record type is invalid.");
            }
            if (frame.Sequence > _nextReceive)
            {
                if (_receiveAhead.ContainsKey(frame.Sequence))
                {
                    return;
                }
                if (_receiveAhead.Count >= ReorderLimit || _receiveAheadBytes + frame.Payload.Length > MaxBuffered)
                {
                    throw new InvalidDataException("HTTP stream sequence is too far ahead.");
                }
                _receiveAhead[frame.Sequence] = frame;
                _receiveAheadBytes += frame.Payload.Length;
                return;
            }
            while (true)
            {
                _nextReceive++;
                _received.Writer.TryWrite(frame);
                if (!_receiveAhead.Remove(_nextReceive, out var next))
                {
                    break;
                }
                _receiveAheadBytes -= next.Payload.Length;
                frame = next;
            }
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
