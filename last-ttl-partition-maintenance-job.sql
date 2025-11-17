use movr;
\x
select * from [show jobs]
where job_type like 'PARTITION TTL SCHEDULER'
and description like '%rides%'
ORDER BY started DESC LIMIT 1;

