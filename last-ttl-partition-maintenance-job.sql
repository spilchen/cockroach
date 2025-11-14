use movr;
\x
select * from [show jobs]
where job_type like 'PARTITION %'
and description like '%rides%'
ORDER BY started DESC LIMIT 1;

